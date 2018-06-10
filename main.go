package main

import (
	"./smtpd"
	"bytes"
	"flag"
	"fmt"
	"github.com/spf13/viper"
	"github.com/veqryn/go-email/email"
	"gopkg.in/telegram-bot-api.v4"
	"log"
	"net"
	"net/smtp"
	"os"
	"strconv"
	"strings"
	"time"
)

const QueueLength = 500

type QueueItem struct {
	from string
	to   []string
	msg  *email.Message
	data []byte
}

var receivers map[string]int64
var bot *tgbotapi.BotAPI
var debug bool
var isFallback bool
var fallbackAuth smtp.Auth
var queues map[int64]chan QueueItem

func main() {
	queues = make(map[int64]chan QueueItem)

	configFilePath := flag.String("c", "./smtp2tg.toml", "Config file location")
	//pidFilePath := flag.String("p", "/var/run/smtp2tg.pid", "Pid file location")
	flag.Parse()

	// Load & parse config
	viper.SetConfigFile(*configFilePath)
	err := viper.ReadInConfig()
	if err != nil {
		log.Fatal(err.Error())
	}
	viper.SetDefault("fallback.user", "")
	viper.SetDefault("fallback.password", "")

	// Logging
	logfile := viper.GetString("logging.file")
	if logfile == "" {
		log.Println("No logging.file defined in config, outputting to stdout")
	} else {
		lf, err := os.OpenFile(logfile, os.O_APPEND|os.O_CREATE|os.O_RDWR, 0666)
		if err != nil {
			log.Fatal(err.Error())
		}
		log.SetOutput(lf)
	}

	// Debug?
	debug = viper.GetBool("logging.debug")

	rawReceivers := viper.GetStringMapString("receivers")
	if rawReceivers["*"] == "" {
		log.Fatal("No wildcard receiver (*) found in config.")
	}
	receivers = make(map[string]int64)
	for address, tgid := range rawReceivers {
		i, err := strconv.ParseInt(tgid, 10, 64)
		if err != nil {
			log.Printf("[ERROR]: wrong telegram id: not int64")
			return
		}
		receivers[address] = i
		queues[i] = make(chan QueueItem, QueueLength)
	}

	var token string = viper.GetString("bot.token")
	if token == "" {
		log.Fatal("No bot.token defined in config")
	}

	var listen string = viper.GetString("smtp.listen")
	var name string = viper.GetString("smtp.name")
	if listen == "" {
		log.Fatal("No smtp.listen defined in config.")
	}
	if name == "" {
		log.Fatal("No smtp.name defined in config.")
	}

	// Initialize TG bot
	bot, err = tgbotapi.NewBotAPI(token)
	if err != nil {
		log.Fatal(err.Error())
	}
	log.Printf("Bot authorized as %s", bot.Self.UserName)

	// Initialize fallback auth
	isFallback = viper.IsSet("fallback.host")
	if isFallback {
		fallbackAuth = smtp.PlainAuth(
			"",
			viper.GetString("fallback.user"),
			viper.GetString("fallback.password"),
			viper.GetString("fallback.host"),
		)
	}

	// Start queue handler
	log.Printf("Started queue handler")
	go handleQueue()

	log.Printf("Initializing smtp server on %s...", listen)
	// Initialize SMTP server
	err_ := smtpd.ListenAndServe(listen, mailHandler, "mail2tg", "", debug)
	if err_ != nil {
		log.Fatal(err_.Error())
	}
}

func mailHandler(origin net.Addr, from string, to []string, data []byte) {

	from = strings.Trim(from, " ><")
	to[0] = strings.Trim(to[0], " ><")
	msg, err := email.ParseMessage(bytes.NewReader(data))
	if err != nil {
		log.Printf("[MAIL ERROR]: %s", err.Error())
		return
	}
	subject := msg.Header.Get("Subject")
	log.Printf("Received mail from '%s' for '%s' with subject '%s'", from, to[0], subject)

	// Find receivers and send to TG
	var tgid = receivers[from]
	if tgid == 0 {
		tgid = receivers["*"]
	}

	textMsgs := msg.MessagesContentTypePrefix("text")
	images := msg.MessagesContentTypePrefix("image")
	if len(textMsgs) == 0 && len(images) == 0 {
		log.Printf("mail doesn't contain text or image")
		return
	}

	log.Printf("Relaying message to: %d", tgid)

	queues[tgid] <- QueueItem{
		from: from,
		to:   to,
		msg:  msg,
		data: data,
	}
}

func handleQueue() {
	var prevQueueLength = 0
	for {
		var queueLength = 0
		for id, items := range queues {
			var item QueueItem
			select {
			case res := <-items:
				item = res
			default:
				continue
			}
			queueLength += len(items)
			subject := item.msg.Header.Get("Subject")
			textMsgs := item.msg.MessagesContentTypePrefix("text")
			images := item.msg.MessagesContentTypePrefix("image")
			if len(textMsgs) > 0 {
				bodyStr := fmt.Sprintf("*%s*\n\n%s", subject, string(textMsgs[0].Body))
				tgMsg := tgbotapi.NewMessage(id, bodyStr)
				tgMsg.ParseMode = tgbotapi.ModeMarkdown
				_, err := bot.Send(tgMsg)
				if err != nil {
					log.Printf("[ERROR]: telegram message send: '%s'", err.Error())
					mailFallback(item.from, item.to, item.data)
					continue
				}
			}
			// TODO Better to use 'sendMediaGroup' to send all attachments as a
			// single message, but go telegram api has not implemented it yet
			// https://github.com/go-telegram-bot-api/telegram-bot-api/issues/143
			for _, part := range images {
				_, params, err := part.Header.ContentDisposition()
				if err != nil {
					log.Printf("[ERROR]: content disposition parse: '%s'", err.Error())
					continue
				}
				text := params["filename"]
				tgFile := tgbotapi.FileBytes{Name: text, Bytes: part.Body}
				tgMsg := tgbotapi.NewPhotoUpload(id, tgFile)
				tgMsg.Caption = text
				// It's not a separate message, so disable notification
				tgMsg.DisableNotification = true
				_, err = bot.Send(tgMsg)
				if err != nil {
					log.Printf("[ERROR]: telegram photo send: '%s'", err.Error())
					continue
				}
			}
		}

		if prevQueueLength != queueLength {
			log.Printf("[INFO]: pending messages: %d", queueLength)
			prevQueueLength = queueLength
		}

		time.Sleep(time.Second)
	}
}

func mailFallback(from string, to []string, data []byte) {
	if !isFallback {
		return
	}
	log.Printf("Sending to fallback email")
	err := smtp.SendMail(
		fmt.Sprintf("%s:%s", viper.GetString("fallback.host"),
			viper.GetString("fallback.port")),
		fallbackAuth,
		from,
		to,
		data,
	)
	if err != nil {
		log.Printf("[ERROR]: fallback mail send", err.Error())
	}
}
