package main

import (
	"github.com/PaulAnnekov/smtp2tg/smtpd"
	"bytes"
	"flag"
	"fmt"
	"github.com/spf13/viper"
	"github.com/veqryn/go-email/email"
	"gopkg.in/telegram-bot-api.v4"
	"log"
	"net"
	"net/smtp"
	"net/mail"
	"os"
	"strconv"
	"strings"
	"time"
	"regexp"
)

const queueLength = 500

type queueItem struct {
	from string
	isPin bool
	to   []string
	msg  *email.Message
	data []byte
}

type address struct {
	Address string
	Tag     string
}

var receivers map[string]int64
var bot *tgbotapi.BotAPI
var debug bool
var isFallback bool
var fallbackAuth smtp.Auth
var queues map[int64]chan queueItem

func main() {
	queues = make(map[int64]chan queueItem)

	configFilePath := flag.String("c", "./smtp2tg.toml", "Config file location")
	//pidFilePath := flag.String("p", "/var/run/smtp2tg.pid", "Pid file location")
	flag.Parse()

	// Load & parse config
	viper.SetConfigFile(*configFilePath)
	err := viper.ReadInConfig()
	if err != nil {
		log.Fatal(err.Error())
	}

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
		queues[i] = make(chan queueItem, queueLength)
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
	if isFallback && viper.IsSet("fallback.user") {
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

func parseAddress(in string) (*address, error) {
	addressParts, err := mail.ParseAddress(in)
	if err != nil {
		return nil, err
	}
	parsedAddress := address{Address: addressParts.Address}
	tagRe := regexp.MustCompile("\\+(.+)@")
	matches := tagRe.FindStringSubmatch(parsedAddress.Address)
	if len(matches)>0 {
		parsedAddress.Tag = matches[1]
		parsedAddress.Address = strings.Replace(parsedAddress.Address, "+"+parsedAddress.Tag, "", 1)
	}
	return &parsedAddress, nil
}

func mailHandler(origin net.Addr, from string, to []string, data []byte) {

	fromAddress, err := parseAddress(from)
	if err != nil {
		log.Printf("[MAIL ERROR]: invalid address '%s': %s", from, err.Error())
		return
	}
	to[0] = strings.Trim(to[0], " ><")
	msg, err := email.ParseMessage(bytes.NewReader(data))
	if err != nil {
		log.Printf("[MAIL ERROR]: %s", err.Error())
		return
	}
	subject := msg.Header.Get("Subject")
	log.Printf("Received mail from '%s' for '%s' with subject '%s'", from, to[0], subject)

	// Find receivers and send to TG
	var tgid = receivers[fromAddress.Address]
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

	queues[tgid] <- queueItem{
		from: fromAddress.Address,
		isPin: fromAddress.Tag=="pin",
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
			var item queueItem
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
				res, err := bot.Send(tgMsg)
				if err != nil {
					log.Printf("[ERROR]: telegram message send: '%s'", err.Error())
					mailFallback(item.from, item.to, item.data)
					continue
				}
				if item.isPin {
					pinConfig := tgbotapi.PinChatMessageConfig{ChatID: id, MessageID: res.MessageID, DisableNotification: true}
					_, err := bot.PinChatMessage(pinConfig)
					if err != nil {
						log.Printf("[ERROR]: telegram pin message: '%s'", err.Error())
					}
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
		log.Printf("[ERROR]: fallback mail send: '%s'", err.Error())
	}
}
