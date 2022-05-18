package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/bwmarrin/discordgo"
)

var (
	message struct {
		Lock sync.Mutex
		Date string
		ID   string
		Body strings.Builder
	}

	config struct {
		BotApi        string `json:"bot_api"`
		GuildID       string `json:"guild_id"`
		TextChannelID string `json:"text_channel_id"`
	}

	ds *discordgo.Session
)

func main() {
	fs, err := os.Open("config.json")
	if err != nil {
		panic(err)
	}
	defer fs.Close()

	err = json.NewDecoder(fs).Decode(&config)
	if err != nil {
		panic(err)
	}
	fs.Close()

	ctx, exit := signal.NotifyContext(
		context.Background(),
		syscall.SIGINT, syscall.SIGTERM,
	)
	defer exit()

	ds, err = discordgo.New("Bot " + config.BotApi)
	if err != nil {
		panic(err)
	}

	ds.Identify.Intents = discordgo.IntentGuildVoiceStates |
		discordgo.IntentGuildMessages |
		discordgo.IntentGuildVoiceStates

	err = ds.Open()
	if err != nil {
		panic(err)
	}
	defer ds.Close()

	getTodayMessageID()

	ds.AddHandler(voiceStatusUpdateEvent)

	<-ctx.Done()
}

func getTodayMessageID() {
	today := time.Now()
	today = time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, today.Location())

	beforeID := ""
	for {
		st, err := ds.ChannelMessages(config.TextChannelID, 100, beforeID, "", "")
		if err != nil {
			panic(err)
		}

		if len(st) == 0 {
			return
		}

		sort.Slice(
			st,
			func(i, j int) bool {
				return st[i].Timestamp.After(st[j].Timestamp)
			},
		)

		for _, msg := range st {
			beforeID = msg.ID

			if msg.Author.ID != ds.State.User.ID {
				continue
			}
			if msg.Timestamp.Before(today) {
				return
			}

			message.Date = today.Format("2006-01-02")
			message.ID = msg.ID
			message.Body.WriteString(msg.Content)

			return
		}
	}
}

func voiceStatusUpdateEvent(sess *discordgo.Session, event *discordgo.VoiceStateUpdate) {
	if event.GuildID != config.GuildID {
		return
	}

	now := time.Now()
	nowTime := now.Format("15:04:05")

	// join
	switch {
	case event.ChannelID == "": // leave
		if event.BeforeUpdate != nil {
			go update(
				now,
				fmt.Sprintf(
					"`%s 퇴장` <@%s> / <#%s>",
					nowTime,
					event.UserID,
					event.BeforeUpdate.ChannelID,
				),
			)
		} else {
			go update(
				now,
				fmt.Sprintf(
					"`%s 퇴장` <@%s>",
					nowTime,
					event.UserID,
				),
			)
		}

	case event.BeforeUpdate == nil: // join
		go update(
			now,
			fmt.Sprintf(
				"`%s 입장` <@%s> / <#%s>",
				nowTime,
				event.UserID,
				event.ChannelID,
			),
		)

	case event.BeforeUpdate.ChannelID != event.ChannelID: // move
		go update(
			now,
			fmt.Sprintf(
				"`%s 이동` <@%s> / <#%s>",
				nowTime,
				event.UserID,
				event.ChannelID,
			),
		)

	default:
		return
	}
}

func update(now time.Time, msg string) {
	message.Lock.Lock()
	defer message.Lock.Unlock()

	today := now.Format("2006-01-02")

	allowedMentions := discordgo.MessageAllowedMentions{}

	if message.Date != today {
		message.Date = today
		message.Body.Reset()
		message.Body.WriteString(now.Format("2006년 01월 02일"))

		msgSend := discordgo.MessageSend{
			Content:         message.Body.String(),
			AllowedMentions: &allowedMentions,
		}
		dsm, err := ds.ChannelMessageSendComplex(config.TextChannelID, &msgSend)
		if err != nil {
			panic(err)
		}

		message.ID = dsm.ID
	}

	message.Body.WriteString("\n")
	message.Body.WriteString(msg)

	content := message.Body.String()
	me := discordgo.MessageEdit{
		Channel:         config.TextChannelID,
		ID:              message.ID,
		Content:         &content,
		AllowedMentions: &allowedMentions,
	}

	_, err := ds.ChannelMessageEditComplex(&me)
	if err != nil {
		panic(err)
	}

	if message.Body.Len() > 1800 {
		message.Date = ""
	}
}
