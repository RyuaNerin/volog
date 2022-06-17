package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/getsentry/sentry-go"
)

const (
	MaxTextLength = 1500
	MaxEmbedUser  = 15
)

var (
	config struct {
		BotApi        string `json:"bot_api"`
		GuildID       string `json:"guild_id"`
		TextChannelID string `json:"text_channel_id"`
		SentryDsn     string `json:"sentry_dsn"`
		Pprof         bool   `json:"pprof"`
	}

	message struct {
		Date string

		LogBody strings.Builder

		LogID       string
		EmbedIDList []string
		Embed       []*statEmbed

		EmbedChannelBuf strings.Builder
		EmbedNameBuf    strings.Builder
		EmbedUptimeBuf  strings.Builder
	}

	discordSession *discordgo.Session

	lock             sync.Mutex
	connectedUserMap = make(map[string]*userInfo, 16)

	// 사전할당
	allowedMentions = discordgo.MessageAllowedMentions{}
	emptyEmbeds     = []*discordgo.MessageEmbed{}

	updateUserQueue = make(chan updateData)

	isBotMapLock sync.RWMutex
	isBotMap     = make(map[string]bool, 16)
)

type updateData struct {
	now time.Time
	msg string
}

type userInfo struct {
	UserID    string    `json:"user_id"`
	ChannelID string    `json:"channel_id"`
	Connected time.Time `json:"connected"`
}

type statEmbed struct {
	StatEmbed discordgo.MessageEmbed

	fieldChannel discordgo.MessageEmbedField
	fieldName    discordgo.MessageEmbedField
	fieldUptime  discordgo.MessageEmbedField
}

func newStatEmbed() *statEmbed {
	stat := &statEmbed{
		fieldChannel: discordgo.MessageEmbedField{
			Name:   "채널",
			Inline: true,
		},
		fieldName: discordgo.MessageEmbedField{
			Name:   "이름",
			Inline: true,
		},
		fieldUptime: discordgo.MessageEmbedField{
			Name:   "접속시간",
			Inline: true,
		},
	}

	stat.StatEmbed = discordgo.MessageEmbed{
		Type:   discordgo.EmbedTypeArticle,
		Footer: &discordgo.MessageEmbedFooter{},
		Fields: []*discordgo.MessageEmbedField{
			&stat.fieldChannel,
			&stat.fieldName,
			&stat.fieldUptime,
		},
	}

	return stat
}

var (
	saveConnectedUserMapLock int32 = 0
)

func saveConnectedUserMap() {
	if atomic.SwapInt32(&saveConnectedUserMapLock, 1) == 1 {
		return
	}
	defer atomic.StoreInt32(&saveConnectedUserMapLock, 0)

	fs, err := os.Create("stat.json")
	if err != nil {
		sentry.CaptureException(err)
		return
	}
	defer fs.Close()

	err = json.NewEncoder(fs).Encode(&connectedUserMap)
	if err != nil {
		sentry.CaptureException(err)
		return
	}
}

func loadConnectedUserMap() {
	fs, err := os.Open("stat.json")
	if err != nil {
		sentry.CaptureException(err)
		return
	}
	defer fs.Close()

	err = json.NewDecoder(fs).Decode(&connectedUserMap)
	if err != nil {
		sentry.CaptureException(err)
		return
	}
}

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

	if config.SentryDsn != "" {
		err := sentry.Init(sentry.ClientOptions{
			Dsn:        config.SentryDsn,
			SampleRate: 1,
		})
		if err != nil {
			panic(err)
		}
	}
	if config.Pprof {
		go http.ListenAndServe("127.0.0.1:57618", http.DefaultServeMux)
	}

	loadConnectedUserMap()

	ctx, exit := signal.NotifyContext(
		context.Background(),
		syscall.SIGINT, syscall.SIGTERM,
	)
	defer exit()

	discordSession, err = discordgo.New("Bot " + config.BotApi)
	if err != nil {
		panic(err)
	}

	discordSession.Identify.Intents = discordgo.IntentGuildVoiceStates |
		discordgo.IntentGuildMessages

	err = discordSession.Open()
	if err != nil {
		panic(err)
	}
	defer discordSession.Close()

	discordSession.AddHandler(memberAddEventHandler)
	discordSession.AddHandler(memberRemoveEventHandler)

	getTodayMessageID()

	go updateUserWorker()
	go updateStatWorker()

	discordSession.AddHandler(voiceStatusUpdateEvent)

	<-ctx.Done()
}

func getTodayMessageID() {
	today := time.Now()
	today = time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, today.Location())

	msgList, err := discordSession.ChannelMessages(config.TextChannelID, 100, "", "", "")
	if err != nil {
		panic(err)
	}

	if len(msgList) > 0 {
		sort.Slice(
			msgList,
			func(i, j int) bool {
				return msgList[i].Timestamp.After(msgList[j].Timestamp)
			},
		)
	}

	var wg sync.WaitGroup

	// 앞쪽 임베딩 사용
	maxEmbeddingIndex := -1
	for i, msg := range msgList {
		if msg.Author == nil || msg.Author.ID != discordSession.State.User.ID {
			continue
		}
		if len(msg.Embeds) == 0 {
			break
		}
		if msg.Timestamp.Before(today) {
			break
		}

		maxEmbeddingIndex = i
		message.EmbedIDList = append(message.EmbedIDList, msg.ID)
	}
	_ = sort.Reverse(sort.StringSlice(message.EmbedIDList))

	// 뒤쪽 임베딩 청소
	for _, msg := range msgList[maxEmbeddingIndex+1:] {
		if msg.Author == nil || msg.Author.ID != discordSession.State.User.ID {
			continue
		}
		if len(msg.Embeds) == 0 {
			continue
		}

		id := msg.ID
		wg.Add(1)
		go func() {
			defer wg.Done()

			err := discordSession.ChannelMessageDelete(config.TextChannelID, id)
			if err != nil {
				sentry.CaptureException(err)
				return
			}
		}()
	}

	wg.Wait()

	todayFormatted := today.Format("2006년 01월 02일")
	for _, msg := range msgList {
		if msg.Author == nil || msg.Author.ID != discordSession.State.User.ID {
			continue
		}
		if len(msg.Embeds) > 0 {
			continue
		}

		if !strings.HasPrefix(msg.Content, todayFormatted) {
			continue
		}

		message.LogBody.WriteString(msg.Content)
		message.Date = todayFormatted
		message.LogID = msg.ID
		return
	}

	msgSend := discordgo.MessageSend{
		AllowedMentions: &allowedMentions,
		Content:         todayFormatted,
	}

	dsm, err := discordSession.ChannelMessageSendComplex(config.TextChannelID, &msgSend)
	if err != nil {
		panic(err)
	}

	message.LogID = dsm.ID
}

func memberAddEventHandler(sess *discordgo.Session, event *discordgo.GuildMemberAdd) {
	if !event.User.Bot {
		return
	}

	isBotMapLock.Lock()
	isBotMap[event.User.ID] = event.User.Bot
	isBotMapLock.Unlock()
}
func memberRemoveEventHandler(sess *discordgo.Session, event *discordgo.GuildMemberRemove) {
	if !event.User.Bot {
		return
	}

	isBotMapLock.Lock()
	delete(isBotMap, event.User.ID)
	isBotMapLock.Unlock()
}

func voiceStatusUpdateEvent(sess *discordgo.Session, event *discordgo.VoiceStateUpdate) {
	if event.GuildID != config.GuildID {
		return
	}

	now := time.Now()

	isBotMapLock.RLock()
	isBot, ok := isBotMap[event.UserID]
	isBotMapLock.RUnlock()

	if !ok {
		go func(guildID, userID string) {
			member, err := discordSession.GuildMember(guildID, userID)
			if err != nil {
				sentry.CaptureException(err)
				return
			}

			isBotMapLock.Lock()
			isBotMap[userID] = member.User.Bot
			isBotMapLock.Unlock()

			if member.User.Bot {
				lock.Lock()
				delete(connectedUserMap, userID)
				lock.Unlock()
			}
		}(
			event.GuildID,
			event.UserID,
		)
	}

	if isBot {
		return
	}

	// join
	switch {
	case event.ChannelID == "": // leave
		if event.BeforeUpdate != nil {
			go userLeave(now, event.UserID, event.BeforeUpdate.ChannelID)
		} else {
			go userLeave(now, event.UserID, "")
		}

	case event.BeforeUpdate == nil: // join
		go userJoin(now, event.UserID, event.ChannelID)

	case event.BeforeUpdate.ChannelID != event.ChannelID: // move
		go userMove(now, event.UserID, event.ChannelID, event.BeforeUpdate.ChannelID)
	}
}

func userJoin(now time.Time, userID, channelID string) {
	lock.Lock()
	u, ok := connectedUserMap[userID]
	if !ok {
		u = &userInfo{
			UserID:    userID,
			ChannelID: channelID,
			Connected: time.Now(),
		}
		connectedUserMap[userID] = u
	}
	go saveConnectedUserMap()
	lock.Unlock()

	updateUserQueue <- updateData{
		now: now,
		msg: fmt.Sprintf(
			"`%s 입장` <@%s> / <#%s>",
			now.Format("15:04:05"),
			userID,
			channelID,
		),
	}
}
func userMove(now time.Time, userID, channelID, channelIDOld string) {
	lock.Lock()
	u, ok := connectedUserMap[userID]
	if !ok {
		u = &userInfo{
			UserID:    userID,
			ChannelID: channelID,
			Connected: time.Now(),
		}
		connectedUserMap[userID] = u
	}
	u.ChannelID = channelID
	go saveConnectedUserMap()
	lock.Unlock()

	updateUserQueue <- updateData{
		now: now,
		msg: fmt.Sprintf(
			"`%s 이동` <@%s> / <#%s>",
			now.Format("15:04:05"),
			userID,
			channelID,
		),
	}
}
func userLeave(now time.Time, userID, channelID string) {
	lock.Lock()
	delete(connectedUserMap, userID)
	go saveConnectedUserMap()
	lock.Unlock()

	var msg string
	if channelID != "" {
		msg = fmt.Sprintf(
			"`%s 퇴장` <@%s> / <#%s>",
			now.Format("15:04:05"),
			userID,
			channelID,
		)
	} else {
		msg = fmt.Sprintf(
			"`%s 퇴장` <@%s>",
			now.Format("15:04:05"),
			userID,
		)
	}

	updateUserQueue <- updateData{
		now: now,
		msg: msg,
	}
}

func updateUserWorker() {
	for {
		ud := <-updateUserQueue

		lock.Lock()
		{
			todayFormatted := ud.now.Format("2006년 01월 02일")

			if message.Date != todayFormatted || message.LogBody.Len() > MaxTextLength {
				message.Date = todayFormatted
				message.LogBody.Reset()
				message.LogBody.WriteString(todayFormatted)

				if len(message.EmbedIDList) == 0 {
					message.LogID = ""
				} else {
					message.LogID = message.EmbedIDList[0]
					if len(message.EmbedIDList) > 1 {
						copy(message.EmbedIDList[0:], message.EmbedIDList[1:])
					}
					message.EmbedIDList = message.EmbedIDList[:len(message.EmbedIDList)-1]
				}
			}

			message.LogBody.WriteString("\n")
			message.LogBody.WriteString(ud.msg)

			content := message.LogBody.String()

			if message.LogID == "" {
				me := discordgo.MessageSend{
					AllowedMentions: &allowedMentions,
					Content:         content,
					Embeds:          emptyEmbeds,
				}
				dsm, err := discordSession.ChannelMessageSendComplex(config.TextChannelID, &me)
				if err != nil {
					sentry.CaptureException(err)
					return
				}
				message.LogID = dsm.ID
				lock.Unlock()
			} else {
				logID := message.LogID
				lock.Unlock()

				me := discordgo.MessageEdit{
					Channel:         config.TextChannelID,
					ID:              logID,
					AllowedMentions: &allowedMentions,
					Content:         &content,
					Embeds:          emptyEmbeds,
				}
				_, err := discordSession.ChannelMessageEditComplex(&me)
				if err != nil {
					sentry.CaptureException(err)
					return
				}
			}
		}
	}
}

func updateStatWorker() {
	time.Sleep(time.Until(time.Now().Truncate(5 * time.Second).Add(5 * time.Second)))

	var locked int32
	t := time.NewTicker(5 * time.Second)
	for {
		<-t.C

		go func() {
			if atomic.SwapInt32(&locked, 1) != 0 {
				return
			}
			defer atomic.StoreInt32(&locked, 0)

			updateStat()
		}()
	}
}

var updateStatTick = false

func updateStat() {
	now := time.Now()

	var w sync.WaitGroup

	lock.Lock()
	userList := make([]*userInfo, 0, 16)
	for _, ud := range connectedUserMap {
		userList = append(userList, ud)
	}
	sort.Slice(
		userList,
		func(i, k int) bool {
			if userList[i].ChannelID == userList[k].ChannelID {
				return userList[i].Connected.Before(userList[k].Connected)
			} else {
				return userList[i].ChannelID < userList[k].ChannelID
			}
		},
	)
	lock.Unlock()

	embedCount := int(math.Ceil(float64(len(userList)) / float64(MaxEmbedUser)))
	if embedCount == 0 {
		embedCount = 1
	}

	for i := len(message.Embed); i < embedCount; i++ {
		message.Embed = append(message.Embed, newStatEmbed())
	}

	// 넘치는거 삭제
	if len(message.EmbedIDList) > embedCount {
		for len(message.EmbedIDList) > embedCount {
			l := len(message.EmbedIDList) - 1
			id := message.EmbedIDList[l]
			message.EmbedIDList = message.EmbedIDList[:l]

			w.Add(1)
			go func() {
				defer w.Done()

				err := discordSession.ChannelMessageDelete(config.TextChannelID, id)
				if err != nil {
					sentry.CaptureException(err)
					return
				}
			}()
		}
		message.EmbedIDList = message.EmbedIDList[:embedCount]
	} else {
		for len(message.EmbedIDList) < embedCount {
			message.EmbedIDList = append(message.EmbedIDList, "")
		}
	}

	updateStatTick = !updateStatTick
	for embedIndex := 0; embedIndex < embedCount; embedIndex++ {
		embed := message.Embed[embedIndex]
		if updateStatTick {
			embed.StatEmbed.Footer.Text = time.Now().Format("⚫ 2006-01-02 15:04:05 기준")
		} else {
			embed.StatEmbed.Footer.Text = time.Now().Format("⚪ 2006-01-02 15:04:05 기준")
		}

		message.EmbedChannelBuf.Reset()
		message.EmbedNameBuf.Reset()
		message.EmbedUptimeBuf.Reset()

		if len(userList) == 0 {
			message.EmbedChannelBuf.WriteString("\u200B")
			message.EmbedNameBuf.WriteString("\u200B")
			message.EmbedUptimeBuf.WriteString("\u200B")
		} else {
			needLineWrap := false
			idxMax := embedIndex*MaxEmbedUser + MaxEmbedUser
			for idx := embedIndex * MaxEmbedUser; idx < idxMax && idx < len(userList); idx++ {
				u := userList[idx]

				ts := now.Sub(u.Connected)

				d := int((ts / time.Hour) / 24)
				h := int((ts / time.Hour) % 24)
				m := int((ts / time.Minute) % 60)

				if needLineWrap {
					message.EmbedChannelBuf.WriteString("\n")
					message.EmbedNameBuf.WriteString("\n")
					message.EmbedUptimeBuf.WriteString("\n")
				}
				needLineWrap = true

				fmt.Fprintf(&message.EmbedChannelBuf, "<#%s>", u.ChannelID)
				fmt.Fprintf(&message.EmbedNameBuf, "<@%s>", u.UserID)
				if d > 0 {
					fmt.Fprintf(&message.EmbedUptimeBuf, "`%2dd %2dh %2dm`", d, h, m)
				} else if h > 0 {
					fmt.Fprintf(&message.EmbedUptimeBuf, "`    %2dh %2dm`", h, m)
				} else {
					fmt.Fprintf(&message.EmbedUptimeBuf, "`        %2dm`", m)
				}
			}
		}

		embed.fieldChannel.Value = message.EmbedChannelBuf.String()
		embed.fieldName.Value = message.EmbedNameBuf.String()
		embed.fieldUptime.Value = message.EmbedUptimeBuf.String()

		if message.EmbedIDList[embedIndex] == "" {
			w.Add(1)
			go func(embedIndex int) {
				defer w.Done()

				msgSend := discordgo.MessageSend{
					AllowedMentions: &allowedMentions,
					Embed:           &embed.StatEmbed,
				}

				dsm, err := discordSession.ChannelMessageSendComplex(config.TextChannelID, &msgSend)
				if err != nil {
					sentry.CaptureException(err)
					return
				}

				message.EmbedIDList[embedIndex] = dsm.ID
			}(embedIndex)
		} else {
			w.Add(1)
			go func(embedIndex int) {
				defer w.Done()

				msgSend := discordgo.MessageEdit{
					Channel:         config.TextChannelID,
					ID:              message.EmbedIDList[embedIndex],
					AllowedMentions: &allowedMentions,
					Embed:           &embed.StatEmbed,
				}

				_, err := discordSession.ChannelMessageEditComplex(&msgSend)
				if err != nil {
					log.Println(err.Error())
				}
			}(embedIndex)
		}
	}

	w.Wait()
}
