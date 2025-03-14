package main

import (
	"database/sql"
	"fmt"
	"io/ioutil"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

type chat struct {
	id             int64
	stellarAccount string
	sanityCheck    time.Time
}

var rwLock sync.RWMutex
var accounts map[string]map[int64]struct{}
var chats map[int64]*chat
var tradePageToken = "now"
var paymentPageToken = "now"

var db *sql.DB

func numAccounts() int {
	return len(accounts)
}

func numChats() int {
	return len(chats)
}

func setTradePageToken(t string) {
	tradePageToken = t
}

func setOperationPageToken(t string) {
	paymentPageToken = t
}

func getTradePageToken() string {
	return tradePageToken
}

func getOperationPageToken() string {
	return paymentPageToken
}

func chatSanity(id int64, d time.Duration) bool {
	rwLock.Lock()
	defer rwLock.Unlock()
	chat := chats[id]
	if chat != nil {
		if time.Since(chat.sanityCheck) > d {
			chat.sanityCheck = time.Now()
			return true
		}
	}
	return false
}

func loadFromStorage() error {
	rwLock.Lock()
	defer rwLock.Unlock()

	rows, err := db.Query("select id, stellar_account from chats;")
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var stellarAccount string
		err = rows.Scan(&id, &stellarAccount)
		if err != nil {
			return err
		}
		chats[id] = &chat{id: id, stellarAccount: stellarAccount}
		if stellarAccount != "" {
			chatIDs := accounts[stellarAccount]
			if chatIDs == nil {
				chatIDs = make(map[int64]struct{})
				accounts[stellarAccount] = chatIDs
			}
			chatIDs[id] = struct{}{}
		}
	}
	err = rows.Err()
	if err != nil {
		return err
	}
	return nil
}

func getChat(id int64) *chat {
	rwLock.RLock()
	defer rwLock.RUnlock()

	return chats[id]
}

func newChat(id int64, account string) error {
	rwLock.Lock()
	defer rwLock.Unlock()

	_, err := db.Exec("insert into chats(id, stellar_account) values(?,?);", id, account)
	if err != nil {
		return err
	}
	chats[id] = &chat{id: id, stellarAccount: account}
	chatIDs := accounts[account]
	if chatIDs == nil {
		chatIDs = make(map[int64]struct{})
		accounts[account] = chatIDs
	}
	chatIDs[id] = struct{}{}
	return nil
}

func deleteChat(id int64) error {
	rwLock.Lock()
	defer rwLock.Unlock()

	chat := chats[id]
	if chat == nil {
		return fmt.Errorf("chat not found")
	}
	_, err := db.Exec("delete from chats where id=?;", chat.id)
	if err != nil {
		return err
	}
	delete(accounts[chat.stellarAccount], id)
	if len(accounts[chat.stellarAccount]) == 0 {
		delete(accounts, chat.stellarAccount)
	}
	delete(chats, id)
	return nil
}

func getChatsByAccount(account string) (result []int64) {
	rwLock.RLock()
	defer rwLock.RUnlock()

	chatIDs := accounts[account]
	if chatIDs == nil {
		return
	}
	for k := range chatIDs {
		result = append(result, k)
	}
	return
}

func setupStorage(f string) (err error) {
	accounts = make(map[string]map[int64]struct{})
	chats = make(map[int64]*chat)

	loadCheckpoint()
	log.Printf("Opening database: %s", f)
	db, err = sql.Open("sqlite3", f)
	if err != nil {
		return
	}

	_, err = db.Exec("create table if not exists chats (id integer not null primary key, stellar_account text);")
	if err != nil {
		return
	}

	err = loadFromStorage()
	if err != nil {
		return
	}

	return
}

func saveCheckpoint() {
	err := ioutil.WriteFile(appPath+"/checkpoint", []byte(fmt.Sprintf("%s,%s,%d", paymentPageToken, tradePageToken, time.Now().Unix())), 0644)
	if err != nil {
		fmt.Printf("Fail saving checkpoint: %v", err)
	} else {
		fmt.Printf("Checkpoint saved")
	}

}

func loadCheckpoint() {
	data, err := ioutil.ReadFile(appPath + "/checkpoint")
	if err != nil {
		return
	}
	chunks := strings.Split(string(data), ",")
	if len(chunks) != 3 {
		return
	}
	ts, err := strconv.ParseInt(chunks[2], 10, 64)
	if err != nil {
		return
	}
	if (time.Now().Unix() - ts) > 300 {
		log.Printf("Checkpoint older than 300 seconds... starting from now")
		return
	}
	paymentPageToken = chunks[0]
	tradePageToken = chunks[1]
	log.Printf("Resume from Payment:%s Trade:%s", paymentPageToken, tradePageToken)
}

func closeStorage() error {
	saveCheckpoint()
	if db != nil {
		db.Close()
		db = nil
	}
	return nil
}
