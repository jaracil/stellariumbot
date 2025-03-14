package main

import (
	"context"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api"
	"github.com/skip2/go-qrcode"
	"github.com/stellar/go/clients/horizonclient"
	"github.com/stellar/go/keypair"
	"github.com/stellar/go/protocols/horizon"
	"github.com/stellar/go/protocols/horizon/operations"
)

type bulkMsg struct {
	chat int64
	text string
}

var version = "0.4"
var bot *tgbotapi.BotAPI
var appPath string
var appCtx context.Context
var appCancel context.CancelFunc
var wg sync.WaitGroup
var bulkChan = make(chan *bulkMsg, 5000)
var fatalErr error
var testBot = false
var bootTime time.Time

const donationAccount = "GB2SWTADAJHQOE5L5DZ5RHFD3G2U6KW7HMDTIZWIWH5WDGZF4AAEAJGX"

func uptime() string {
	e := time.Since(bootTime)
	days := e / (time.Hour * 24)
	e -= days * time.Hour * 24
	hours := e / time.Hour
	e -= hours * time.Hour
	minutes := e / time.Minute
	e -= minutes * time.Minute

	return fmt.Sprintf("%d Days %d Hours %d Minutes", days, hours, minutes)
}

func accountInfo(addr string) (horizon.Account, error) {
	client := horizonclient.DefaultPublicNetClient

	accountRequest := horizonclient.AccountRequest{AccountID: addr}
	return client.AccountDetail(accountRequest)
}

func walletInfo(addr string) (string, error) {
	ai, err := accountInfo(addr)
	if err != nil {
		return "", fmt.Errorf("Error obtaining wallet info")
	}
	ret := ""
	ret += fmt.Sprintf("Wallet address: %s\n", addr)
	ret += fmt.Sprintf("Balances:\n")
	for _, balance := range ai.Balances {
		code := balance.Code
		typ := balance.Type
		if typ == "native" {
			code = "XLM"
		}
		ret += fmt.Sprintf("%s: %s\n", code, balance.Balance)
	}
	return ret, nil
}

func sendAction(chat int64, action string) (err error) {
	ac := tgbotapi.NewChatAction(chat, action)
	_, err = bot.Send(ac)
	return
}

func sendMessage(chat int64, text string) error {
	if chatSanity(chat, time.Hour*24) {
		if chat < 0 { // Groups
			n, err := bot.GetChatMembersCount(tgbotapi.ChatConfig{ChatID: chat})
			if err == nil {
				if n < 2 {
					log.Printf("Bot left alone. Autoremove chat:%d", chat)
					deleteChat((chat))
					return nil
				}
			}
		}
	}

	if len(text) > 4000 {
		text = text[:4000]
		text += "\n... truncated"
	}

	log.Printf("Sent to chat:%d text:%s", chat, strconv.Quote(text))
	mc := tgbotapi.NewMessage(chat, text)
	_, err := bot.Send(mc)
	if err != nil {
		log.Printf("error sending message to chat:%d error:%T:%s", chat, err, err.Error())
		if strings.Contains(err.Error(), "Bad Request") || strings.Contains(err.Error(), "Forbidden") {
			log.Printf("Autoremove chat:%d", chat)
			deleteChat((chat))
			return nil
		}
		return err
	}
	return nil
}

func sendHelp(chat int64) error {
	text := "/start stellar_address -> Starts bot\n/stop -> Stops bot\n/info -> Shows account info\n/qr -> Shows stellar address QR code\n/help -> This help\n/donate -> Donation account\nChat: https://t.me/stellariumchat"
	return sendMessage(chat, text)
}

func sendMessageBulk(chat int64, text string) {
	bulkChan <- &bulkMsg{chat: chat, text: text}
}

func bulkThread() {
	touchMap := make(map[int64]time.Time)
	msgMap := make(map[int64][]string)

	defer wg.Done()

	done := false
	for !done {
		select {
		case msg := <-bulkChan:
			if len(msgMap[msg.chat]) < 20 {
				msgMap[msg.chat] = append(msgMap[msg.chat], msg.text)
				touchMap[msg.chat] = time.Now()
			} else {
				if len(msgMap[msg.chat]) == 20 {
					msgMap[msg.chat] = append(msgMap[msg.chat], "... too many messages")
				}
			}
		case <-appCtx.Done():
			done = true
		default:
			for k, v := range msgMap {
				if time.Since(touchMap[k]) >= 2000*time.Millisecond {
					text := strings.Join(v, "\n\n")
					sendMessage(k, text)
					delete(touchMap, k)
					delete(msgMap, k)
				}
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
	log.Printf("bulkThread goroutine done")
}

func dispatchMessage(msg *tgbotapi.Message) {

	if msg == nil {
		log.Printf("Received NIL message")
		return
	}

	chat := getChat(msg.Chat.ID)

	if !strings.HasPrefix(msg.Text, "/") { //Not a command
		return
	}

	chunks := strings.SplitN(msg.Text, " ", 2)
	command := strings.Trim(strings.ToLower(strings.SplitN(chunks[0], "@", 2)[0]), " ")
	param := ""
	if len(chunks) > 1 {
		param = strings.Trim(chunks[1], " ")
	}

	log.Printf("Received from chat:%d text:%s", msg.Chat.ID, strconv.Quote(msg.Text))
	switch command {
	case "/help":
		sendHelp(msg.Chat.ID)

	case "/start":
		if chat != nil {
			sendMessage(msg.Chat.ID, "Bot already started.")
			if param != "" {
				sendMessage(msg.Chat.ID, "If you want change tracking account, send /stop first")
			} else {
				sendMessage(msg.Chat.ID, fmt.Sprintf("Tracking Stellar address: %s", chat.stellarAccount))
			}
			return
		}

		if param == "" {
			sendMessage(msg.Chat.ID, "Stellar address not provided")
			sendHelp(msg.Chat.ID)
			return
		}
		stellarAddress := param
		_, err := keypair.Parse(stellarAddress)
		if err != nil {
			sendMessage(msg.Chat.ID, "Invalid Stellar address")
			return
		}
		err = newChat(msg.Chat.ID, stellarAddress)
		if err != nil {
			sendMessage(msg.Chat.ID, "Sorry, Internal error")
			return
		}
		log.Printf("New tracking Chat:%d Address:%s", msg.Chat.ID, stellarAddress)
		sendMessage(msg.Chat.ID, fmt.Sprintf("Tracking Stellar address: %s", stellarAddress))

	case "/stop":
		if chat == nil {
			sendMessage(msg.Chat.ID, "Bot already stopped")
			return
		}
		err := deleteChat(msg.Chat.ID)
		if err != nil {
			sendMessage(msg.Chat.ID, "Sorry, Internal error")
			return
		}
		sendMessage(msg.Chat.ID, "Bot stopped")

	case "/donate":
		png, err := qrcode.Encode(donationAccount, qrcode.Medium, -4)
		if err != nil {
			if err != nil {
				sendMessage(msg.Chat.ID, "Sorry, Internal error")
				return
			}
		}
		img := tgbotapi.NewPhotoUpload(msg.Chat.ID, tgbotapi.FileBytes{Bytes: png, Name: "donation.png"})
		img.Caption = "Donation account:\n" + donationAccount
		bot.Send(img)

	case "/info":
		if chat == nil {
			sendMessage(msg.Chat.ID, "You must /start bot first")
			return
		}
		ret, err := walletInfo((chat.stellarAccount))
		if err != nil {
			sendMessage(msg.Chat.ID, "Error obtaining wallet info")
			return
		}
		sendMessage(msg.Chat.ID, ret)

	case "/stats":
		sendMessage(msg.Chat.ID, fmt.Sprintf("Uptime: %s\nAccounts: %d\nChats: %d", uptime(), numAccounts(), numChats()))

	case "/qr":
		if chat == nil {
			sendMessage(msg.Chat.ID, "You must /start bot first")
			return
		}
		png, err := qrcode.Encode(chat.stellarAccount, qrcode.Medium, -4)
		if err != nil {
			if err != nil {
				sendMessage(msg.Chat.ID, "Sorry, Internal error")
				return
			}
		}
		img := tgbotapi.NewPhotoUpload(msg.Chat.ID, tgbotapi.FileBytes{Bytes: png, Name: "wallet.png"})
		img.Caption = chat.stellarAccount
		bot.Send(img)

	default:
		sendMessage(msg.Chat.ID, "Invalid command")
		sendHelp(msg.Chat.ID)
	}

}

func dispatchOperation(operation operations.Operation) {
	if !operation.IsTransactionSuccessful() {
		return
	}

	setOperationPageToken(operation.PagingToken())

	switch op := operation.(type) {

	case operations.Payment:
		//log.Printf("From:%s To:%s Amount:%s", payment.From, payment.To, payment.Amount)
		chats := getChatsByAccount(op.From)
		if chats != nil {
			asset := op.Asset.Code
			if op.Asset.Type == "native" {
				asset = "XLM"
			}
			str := fmt.Sprintf("Sent %s %s to %s", op.Amount, asset, op.To)
			if op.Transaction != nil {
				if op.Transaction.MemoType == "text" && op.Transaction.Memo != "" {
					str += fmt.Sprintf("\nMemo: %s", op.Transaction.Memo)
				}
			}
			v, err := strconv.ParseFloat(op.Amount, 64)
			if err != nil {
				return
			}
			if asset == "XLM" && v < 0.001 {
				log.Printf("SPAM: %s", str)
				return
			}
			for _, chatID := range chats {
				sendMessageBulk(chatID, str)
			}
		}

		chats = getChatsByAccount(op.To)
		if chats != nil {
			asset := op.Asset.Code
			if op.Asset.Type == "native" {
				asset = "XLM"
			}
			str := fmt.Sprintf("Received %s %s from %s", op.Amount, asset, op.From)
			if op.Transaction != nil {
				if op.Transaction.MemoType == "text" && op.Transaction.Memo != "" {
					str += fmt.Sprintf("\nMemo: %s", op.Transaction.Memo)
				}
			}
			v, err := strconv.ParseFloat(op.Amount, 64)
			if err != nil {
				return
			}
			if asset == "XLM" && v < 0.001 {
				log.Printf("SPAM: %s", str)
				return
			}			
			for _, chatID := range chats {
				sendMessageBulk(chatID, str)
			}
		}
	case operations.CreateAccount:
		chats := getChatsByAccount(op.Funder)
		if chats != nil {
			str := fmt.Sprintf("Create account %s with %s XLM", op.Account, op.StartingBalance)
			if op.Transaction != nil {
				if op.Transaction.MemoType == "text" && op.Transaction.Memo != "" {
					str += fmt.Sprintf("\nMemo: %s", op.Transaction.Memo)
				}
			}
			for _, chatID := range chats {
				sendMessageBulk(chatID, str)
			}
		}

		chats = getChatsByAccount(op.Account)
		if chats != nil {
			str := fmt.Sprintf("Account created by funder %s with %s XLM", op.Funder, op.StartingBalance)
			if op.Transaction != nil {
				if op.Transaction.MemoType == "text" && op.Transaction.Memo != "" {
					str += fmt.Sprintf("\nMemo: %s", op.Transaction.Memo)
				}
			}
			for _, chatID := range chats {
				sendMessageBulk(chatID, str)
			}
		}

	}
}

func dispatchTrade(trade horizon.Trade) {
	setTradePageToken(trade.PagingToken())
	chats := getChatsByAccount(trade.BaseAccount)
	if chats != nil {
		baseAsset := trade.BaseAssetCode
		if trade.BaseAssetType == "native" {
			baseAsset = "XLM"
		}
		counterAsset := trade.CounterAssetCode
		if trade.CounterAssetType == "native" {
			counterAsset = "XLM"
		}
		baseVal, err := strconv.ParseFloat(trade.BaseAmount, 64)
		if err != nil {
			log.Printf("Error parsing base amount:%v", err)
			return
		}
		counterVal, err := strconv.ParseFloat(trade.CounterAmount, 64)
		if err != nil {
			log.Printf("Error parsing counter amount:%v", err)
			return
		}

		for _, chatID := range chats {
			str := fmt.Sprintf("Bought %s %s for %s %s\nPrice: %f %s/%s", trade.CounterAmount, counterAsset, trade.BaseAmount, baseAsset, baseVal/counterVal, baseAsset, counterAsset)
			sendMessageBulk(chatID, str)
		}
	}

	chats = getChatsByAccount(trade.CounterAccount)
	if chats != nil {
		baseAsset := trade.BaseAssetCode
		if trade.BaseAssetType == "native" {
			baseAsset = "XLM"
		}
		counterAsset := trade.CounterAssetCode
		if trade.CounterAssetType == "native" {
			counterAsset = "XLM"
		}
		baseVal, err := strconv.ParseFloat(trade.BaseAmount, 64)
		if err != nil {
			log.Printf("Error parsing base amount:%v", err)
			return
		}
		counterVal, err := strconv.ParseFloat(trade.CounterAmount, 64)
		if err != nil {
			log.Printf("Error parsing counter amount:%v", err)
			return
		}

		for _, chatID := range chats {
			str := fmt.Sprintf("Sold %s %s for %s %s\nPrice: %f %s/%s", trade.CounterAmount, counterAsset, trade.BaseAmount, baseAsset, baseVal/counterVal, baseAsset, counterAsset)
			sendMessageBulk(chatID, str)
		}
	}
}

func setupHorizon() error {
	// client := horizonclient.DefaultPublicNetClient
	client := &horizonclient.Client{
		HorizonURL: "http://127.0.0.1:8000",
		//HorizonURL: "https://stellar-horizon.satoshipay.io/",
		//HorizonURL: "https://horizon.stellar.lobstr.co/",
		//HorizonURL: "https://horizon.stellar.org/",
		HTTP: http.DefaultClient,
	}

	// Payments
	// wg.Add(1)
	go func() {
		// defer wg.Done()
		cnt := 0
		for {
			start := time.Now()
			opRequest := horizonclient.OperationRequest{Cursor: getOperationPageToken(), Join: "transactions"}
			err := client.StreamOperations(appCtx, opRequest, dispatchOperation)
			if err != nil {
				log.Printf("Horizon payments thread failure (%v) attempt %d", err, cnt)
			}
			if appCtx.Err() != nil {
				break
			}
			if time.Since(start) < 30*time.Second {
				cnt++
			} else {
				cnt = 0
			}
			if cnt > 10 {
				//appCancel()
				time.Sleep(30*time.Second)
				//break
			}
		}
		log.Printf("Horizon payment gorroutine done")
	}()

	// Trades
	// wg.Add(1)
	go func() {
		// defer wg.Done()
		cnt := 0
		for {
			start := time.Now()
			trRequest := horizonclient.TradeRequest{Cursor: getOperationPageToken()}

			err := client.StreamTrades(appCtx, trRequest, dispatchTrade)
			if err != nil {
				log.Printf("Horizon trades thread failure (%v) attempt %d", err, cnt)
			}
			if appCtx.Err() != nil {
				break
			}
			if time.Since(start) < 30*time.Second {
				cnt++
			} else {
				cnt = 0
			}
			if cnt > 10 {
				//appCancel()
				time.Sleep(30*time.Second)
				//break
			}
		}
		log.Printf("Horizon trades gorroutine done")
	}()

	return nil
}

func setupBot(botKeyFile string) error {
	var err error

	log.Printf("Opening bot key file: %s", botKeyFile)
	data, err := ioutil.ReadFile(botKeyFile)
	if err != nil {
		return err
	}
	botToken := string(data)
	botToken = strings.Trim(botToken, " \r\n\t")

	bot, err = tgbotapi.NewBotAPI(botToken)
	if err != nil {
		return err
	}
	bot.Debug = false

	log.Printf("Telegram bot account %s", bot.Self.UserName)

	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60

	updates, err := bot.GetUpdatesChan(u)
	if err != nil {
		return err
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		done := false
		for !done {
			select {
			case update := <-updates:
				go dispatchMessage(update.Message)
			case <-appCtx.Done():
				done = true
			}
		}
		log.Printf("Telegram bot goroutine done")
	}()

	wg.Add(1)
	go bulkThread()

	return nil
}

func setupCloseHandler() error {
	c := make(chan os.Signal)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGKILL)
	go func() {
		signal := <-c
		log.Printf("Received %v signal, leaving...", signal)
		appCancel()
	}()
	return nil
}

func exit() {
	if fatalErr != nil {
		log.Printf("Fatal error:%v", fatalErr)
	}
	closeStorage()
	log.Printf("Waiting all gorroutines done")
	wg.Wait()
}

func main() {
	var err error
	bootTime = time.Now()

	log.Printf("Stellarium bot version: %s", version)

	appPath, err = filepath.Abs(os.Args[0])
	if err != nil {
		log.Fatal("Can't get absolute path")
	}
	appPath = filepath.Dir(appPath)

	defer exit()

	flag.BoolVar(&testBot, "test", false, "Test mode")
	flag.Parse()

	if testBot {
		log.Printf("Running in test mode")
	}

	appCtx, appCancel = context.WithCancel(context.Background())
	fatalErr = setupCloseHandler()
	if fatalErr != nil {
		return
	}

	if testBot {
		fatalErr = setupStorage(appPath + "/stellarium-test.db")
	} else {
		fatalErr = setupStorage(appPath + "/stellarium.db")
	}
	if fatalErr != nil {
		return
	}

	if testBot {
		fatalErr = setupBot(appPath + "/bot-test.key")
	} else {
		fatalErr = setupBot(appPath + "/bot.key")
	}

	if fatalErr != nil {
		return
	}
	fatalErr = setupHorizon()
	if fatalErr != nil {
		return
	}

	<-appCtx.Done()

}
