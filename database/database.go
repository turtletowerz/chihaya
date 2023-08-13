/*
 * This file is part of Chihaya.
 *
 * Chihaya is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * Chihaya is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with Chihaya.  If not, see <http://www.gnu.org/licenses/>.
 */

package database

import (
	"bytes"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"chihaya/collectors"
	"chihaya/config"
	cdb "chihaya/database/types"
	"chihaya/util"

	"github.com/go-sql-driver/mysql"
)

type Connection struct {
	sqlDb *sql.DB
	mutex sync.Mutex
}

type Database struct {
	snatchChannel          chan *bytes.Buffer
	transferHistoryChannel chan *bytes.Buffer
	transferIpsChannel     chan *bytes.Buffer
	torrentChannel         chan *bytes.Buffer
	userChannel            chan *bytes.Buffer

	loadTorrentsStmt              *sql.Stmt
	loadTorrentGroupFreeleechStmt *sql.Stmt
	loadClientsStmt               *sql.Stmt
	loadFreeleechStmt             *sql.Stmt
	loadHnrStmt                   *sql.Stmt
	loadUsersStmt                 *sql.Stmt
	cleanStalePeersStmt           *sql.Stmt
	unPruneTorrentStmt            *sql.Stmt

	Users                 atomic.Pointer[map[string]*cdb.User]
	HitAndRuns            atomic.Pointer[map[cdb.UserTorrentPair]struct{}]
	Torrents              atomic.Pointer[map[cdb.TorrentHash]*cdb.Torrent]
	TorrentGroupFreeleech atomic.Pointer[map[cdb.TorrentGroupKey]*cdb.TorrentGroupFreeleech]
	Clients               atomic.Pointer[map[uint16]string]

	mainConn *Connection // Used for reloading and misc queries

	bufferPool *util.BufferPool

	transferHistoryLock sync.Mutex

	terminate bool
	waitGroup sync.WaitGroup
}

var (
	deadlockWaitTime   int
	maxDeadlockRetries int
)

var defaultDsn = map[string]string{
	"username": "chihaya",
	"password": "",
	"proto":    "tcp",
	"addr":     "127.0.0.1:3306",
	"database": "chihaya",
}

func (db *Database) Init() {
	db.terminate = false

	slog.Info("opening database connection")

	db.mainConn = Open()

	// Used for recording updates, so the max required size should be < 128 bytes. See queue.go for details
	db.bufferPool = util.NewBufferPool(128)

	var err error

	db.loadUsersStmt, err = db.mainConn.sqlDb.Prepare(
		"SELECT ID, torrent_pass, DownMultiplier, UpMultiplier, DisableDownload, TrackerHide " +
			"FROM users_main WHERE Enabled = '1'")
	if err != nil {
		panic(err)
	}

	db.loadHnrStmt, err = db.mainConn.sqlDb.Prepare(
		"SELECT h.uid, h.fid FROM transfer_history AS h " +
			"JOIN users_main AS u ON u.ID = h.uid WHERE h.hnr = 1 AND u.Enabled = '1'")
	if err != nil {
		panic(err)
	}

	db.loadTorrentsStmt, err = db.mainConn.sqlDb.Prepare(
		"SELECT ID, info_hash, DownMultiplier, UpMultiplier, Snatched, Status, GroupID, TorrentType FROM torrents " +
			"WHERE TorrentType != 'internal'")
	if err != nil {
		panic(err)
	}

	db.loadTorrentGroupFreeleechStmt, err = db.mainConn.sqlDb.Prepare(
		"SELECT GroupID, `Type`, DownMultiplier, UpMultiplier FROM torrent_group_freeleech")
	if err != nil {
		panic(err)
	}

	db.loadClientsStmt, err = db.mainConn.sqlDb.Prepare(
		"SELECT id, peer_id FROM approved_clients WHERE archived = 0")
	if err != nil {
		panic(err)
	}

	db.loadFreeleechStmt, err = db.mainConn.sqlDb.Prepare(
		"SELECT mod_setting FROM mod_core WHERE mod_option = 'global_freeleech'")
	if err != nil {
		panic(err)
	}

	db.cleanStalePeersStmt, err = db.mainConn.sqlDb.Prepare(
		"UPDATE transfer_history SET active = 0 WHERE last_announce < ? AND active = 1")
	if err != nil {
		panic(err)
	}

	db.unPruneTorrentStmt, err = db.mainConn.sqlDb.Prepare(
		"UPDATE torrents SET Status = 0 WHERE ID = ?")
	if err != nil {
		panic(err)
	}

	dbUsers := make(map[string]*cdb.User)
	db.Users.Store(&dbUsers)

	dbTorrents := make(map[cdb.TorrentHash]*cdb.Torrent)
	db.Torrents.Store(&dbTorrents)

	dbHitAndRuns := make(map[cdb.UserTorrentPair]struct{})
	db.HitAndRuns.Store(&dbHitAndRuns)

	dbClients := make(map[uint16]string)
	db.Clients.Store(&dbClients)

	db.deserialize()

	// Run initial load to populate data in memory before we start accepting connections
	slog.Info("populating initial data into memory")
	db.loadUsers()
	db.loadHitAndRuns()
	db.loadTorrents()
	db.loadGroupsFreeleech()
	db.loadConfig()
	db.loadClients()

	slog.Info("starting goroutines")
	db.startReloading()
	db.startSerializing()
	db.startFlushing()
}

func (db *Database) Terminate() {
	slog.Info("terminating database connection")

	db.terminate = true

	slog.Info("closing all flush channels")
	db.closeFlushChannels()

	go func() {
		time.Sleep(10 * time.Second)
		slog.Info("waiting for database flushing to finish")
	}()

	db.waitGroup.Wait()
	db.mainConn.mutex.Lock()
	_ = db.mainConn.Close()
	db.mainConn.mutex.Unlock()
	db.serialize()
}

func Open() *Connection {
	databaseConfig := config.Section("database")
	deadlockWaitTime, _ = databaseConfig.GetInt("deadlock_pause", 1)
	maxDeadlockRetries, _ = databaseConfig.GetInt("deadlock_retries", 5)

	channelsConfig := config.Section("channels")
	torrentFlushBufferSize, _ = channelsConfig.GetInt("torrent", 5000)
	userFlushBufferSize, _ = channelsConfig.GetInt("user", 5000)
	transferHistoryFlushBufferSize, _ = channelsConfig.GetInt("transfer_history", 5000)
	transferIpsFlushBufferSize, _ = channelsConfig.GetInt("transfer_ips", 5000)
	snatchFlushBufferSize, _ = channelsConfig.GetInt("snatch", 25)

	// DSN Format: username:password@protocol(address)/dbname?param=value
	// First try to load the DSN from environment. USeful for tests.
	databaseDsn := os.Getenv("DB_DSN")
	if databaseDsn == "" {
		dbUsername, _ := databaseConfig.Get("username", defaultDsn["username"])
		dbPassword, _ := databaseConfig.Get("password", defaultDsn["password"])
		dbProto, _ := databaseConfig.Get("proto", defaultDsn["proto"])
		dbAddr, _ := databaseConfig.Get("addr", defaultDsn["addr"])
		dbDatabase, _ := databaseConfig.Get("database", defaultDsn["database"])
		databaseDsn = fmt.Sprintf("%s:%s@%s(%s)/%s",
			dbUsername,
			dbPassword,
			dbProto,
			dbAddr,
			dbDatabase,
		)
	}

	sqlDb, err := sql.Open("mysql", databaseDsn)
	if err != nil {
		panic(err)
	}

	if err = sqlDb.Ping(); err != nil {
		panic(err)
	}

	return &Connection{
		sqlDb: sqlDb,
	}
}

func (db *Connection) Close() error {
	return db.sqlDb.Close()
}

func (db *Connection) query(stmt *sql.Stmt, args ...interface{}) *sql.Rows { //nolint:unparam
	rows, _ := perform(func() (interface{}, error) {
		return stmt.Query(args...)
	}).(*sql.Rows)

	return rows
}

func (db *Connection) execute(stmt *sql.Stmt, args ...interface{}) sql.Result {
	result, _ := perform(func() (interface{}, error) {
		return stmt.Exec(args...)
	}).(sql.Result)

	return result
}

func (db *Connection) exec(query *bytes.Buffer, args ...interface{}) sql.Result { //nolint:unparam
	result, _ := perform(func() (interface{}, error) {
		return db.sqlDb.Exec(query.String(), args...)
	}).(sql.Result)

	return result
}

func perform(exec func() (interface{}, error)) (result interface{}) {
	var (
		err   error
		tries int
		wait  time.Duration
	)

	for tries = 1; tries <= maxDeadlockRetries; tries++ {
		result, err = exec()
		if err != nil {
			//goland:noinspection GoTypeAssertionOnErrors
			if merr, isMysqlError := err.(*mysql.MySQLError); isMysqlError {
				if merr.Number == 1213 || merr.Number == 1205 {
					wait = time.Duration(deadlockWaitTime*tries) * time.Second
					slog.Warn("dedlock found", "wait", wait.String(), "try", tries, "max", maxDeadlockRetries)

					if tries == 1 {
						collectors.IncrementDeadlockCount()
					}

					collectors.IncrementDeadlockTime(wait)
					time.Sleep(wait)

					continue
				}

				slog.Error("sql error found", "err", merr.Number, "msg", merr.Message)
				collectors.IncrementSQLErrorCount()
			} else {
				panic(err)
			}
		}

		return
	}

	slog.Error("deadlock retries exceeded", "tries", tries)
	collectors.IncrementDeadlockAborted()

	return
}
