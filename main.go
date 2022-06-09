package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
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
	}

	message struct {
		Lock sync.Mutex
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

	connectedUserMapLock sync.Mutex
	connectedUserMap     = make(map[string]*userInfo, 16)

	updateStatSignal = make(chan struct{}, 1)

	// 사전할당
	allowedMentions = discordgo.MessageAllowedMentions{}
	emptyEmbeds     = []*discordgo.MessageEmbed{}

	updateUserQueue = make(chan updateData)
)

type updateData struct {
	now time.Time
	msg string
}

type userInfo struct {
	id        string
	channelID string
	connected time.Time
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

func main() {
	fs, err := os.Open("config.json")
	if err != nil {
		panic(err)
	}
	defer fs.Close()

	if config.SentryDsn != "" {
		err := sentry.Init(sentry.ClientOptions{
			Dsn: config.SentryDsn,
		})
		if err != nil {
			panic(err)
		}
	}

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

	getTodayMessageID()

	go updateUserWorker()
	go updateStatWorker()

	discordSession.AddHandler(voiceStatusUpdateEvent)

	<-ctx.Done()
}

func updateStatNow() {
	select {
	case updateStatSignal <- struct{}{}:
	default:
	}
}

func getTodayMessageID() {
	today := time.Now()
	today = time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, today.Location())

	idList := make([]*discordgo.Message, 0, 100)

	beforeID := ""
	for {
		st, err := discordSession.ChannelMessages(config.TextChannelID, 100, beforeID, "", "")
		if err != nil {
			panic(err)
		}

		if len(st) == 0 {
			break
		}

		sort.Slice(
			st,
			func(i, j int) bool {
				return st[i].Timestamp.After(st[j].Timestamp)
			},
		)

		for _, msg := range st {
			beforeID = msg.ID

			if msg.Author == nil {
				continue
			}

			if msg.Author.ID != discordSession.State.User.ID {
				continue
			}
			if msg.Timestamp.Before(today) {
				goto ret
			}

			idList = append(idList, msg)
		}
		if len(idList) > 2 {
			goto ret
		}
	}

ret:
	if len(idList) == 0 {
		return
	}

	sort.Slice(
		idList,
		func(i, k int) bool {
			return idList[i].ID > idList[k].ID
		},
	)

	var wg sync.WaitGroup

	// 앞쪽 임베딩 사용
	maxEmbeddingIndex := -1
	for i, msg := range idList {
		if len(msg.Embeds) == 0 {
			break
		}
		maxEmbeddingIndex = i
		message.EmbedIDList = append(message.EmbedIDList, msg.ID)
	}
	_ = sort.Reverse(sort.StringSlice(message.EmbedIDList))

	// 뒤쪽 임베딩 청소
	for idx := maxEmbeddingIndex + 1; idx < len(idList); idx++ {
		if len(idList[idx].Embeds) == 0 {
			continue
		}

		id := idList[idx].ID
		wg.Add(1)
		go func() {
			defer wg.Done()

			err := discordSession.ChannelMessageDelete(config.TextChannelID, id)
			if err != nil {
				sentry.CaptureException(err)
			}
		}()
	}

	wg.Wait()

	for _, msg := range idList {
		if len(msg.Embeds) > 0 {
			continue
		}

		message.LogBody.WriteString(msg.Content)
		message.Date = today.Format("2006-01-02")
		message.LogID = msg.ID
		return
	}

	msgSend := discordgo.MessageSend{
		AllowedMentions: &allowedMentions,
		Content:         today.Format("2006-01-02"),
	}

	dsm, err := discordSession.ChannelMessageSendComplex(config.TextChannelID, &msgSend)
	if err != nil {
		panic(err)
	}

	message.LogID = dsm.ID
}

func voiceStatusUpdateEvent(sess *discordgo.Session, event *discordgo.VoiceStateUpdate) {
	if event.GuildID != config.GuildID {
		return
	}

	now := time.Now()

	// join
	switch {
	case event.ChannelID == "": // leave
		if event.BeforeUpdate != nil {
			userLeave(now, event.UserID, event.BeforeUpdate.ChannelID)
		} else {
			userLeave(now, event.UserID, "")
		}

	case event.BeforeUpdate == nil: // join
		userJoin(now, event.UserID, event.ChannelID)

	case event.BeforeUpdate.ChannelID != event.ChannelID: // move
		userMove(now, event.UserID, event.ChannelID, event.BeforeUpdate.ChannelID)

	default:
		return
	}
}

func userJoin(now time.Time, userID, channelID string) {
	connectedUserMapLock.Lock()
	defer connectedUserMapLock.Unlock()

	u, ok := connectedUserMap[userID]
	if !ok {
		u = &userInfo{
			id:        userID,
			channelID: channelID,
			connected: time.Now(),
		}
		connectedUserMap[userID] = u
	}

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
	connectedUserMapLock.Lock()
	defer connectedUserMapLock.Unlock()

	u, ok := connectedUserMap[userID]
	if !ok {
		u = &userInfo{
			id:        userID,
			channelID: channelID,
			connected: time.Now(),
		}
		connectedUserMap[userID] = u
	}
	u.channelID = channelID

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
	connectedUserMapLock.Lock()
	defer connectedUserMapLock.Unlock()

	delete(connectedUserMap, userID)

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

		message.Lock.Lock()
		{
			today := ud.now.Format("2006-01-02")

			if message.Date != today || message.LogBody.Len() > MaxTextLength {
				message.Date = today
				message.LogBody.Reset()
				message.LogBody.WriteString(ud.now.Format("2006년 01월 02일"))

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
				}
				message.LogID = dsm.ID
			} else {
				me := discordgo.MessageEdit{
					Channel:         config.TextChannelID,
					ID:              message.LogID,
					AllowedMentions: &allowedMentions,
					Content:         &content,
					Embeds:          emptyEmbeds,
				}
				_, err := discordSession.ChannelMessageEditComplex(&me)
				if err != nil {
					sentry.CaptureException(err)
				}
			}
		}
		message.Lock.Unlock()

		updateStatNow()
	}
}

func updateStatWorker() {
	next := time.Now().Truncate(5 * time.Second).Add(5 * time.Second)
	for {
		ds := time.Until(next)
		if ds > 0 {
			select {
			case <-time.After(ds):
			case <-updateStatSignal:
			}
		}

		select {
		case <-updateStatSignal:
		default:
		}

		updateStat()

		next = next.Add(5 * time.Second)
	}
}

var updateStatTick = false

func updateStat() {
	message.Lock.Lock()
	defer message.Lock.Unlock()

	connectedUserMapLock.Lock()
	defer connectedUserMapLock.Unlock()

	var w sync.WaitGroup

	now := time.Now()

	userList := make([]*userInfo, 0, 16)
	for _, ud := range connectedUserMap {
		userList = append(userList, ud)
	}
	sort.Slice(
		userList,
		func(i, k int) bool {
			if userList[i].channelID == userList[k].channelID {
				return userList[i].connected.Before(userList[k].connected)
			} else {
				return userList[i].channelID < userList[k].channelID
			}
		},
	)

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

		needLineWrap := false
		idxMax := embedIndex*MaxEmbedUser + MaxEmbedUser
		for idx := embedIndex * MaxEmbedUser; idx < idxMax && idx < len(userList); idx++ {
			u := userList[idx]

			ts := now.Sub(u.connected)

			h := (ts / time.Hour)
			m := (ts % time.Hour) / time.Minute
			s := (ts % time.Minute) / time.Second

			if needLineWrap {
				message.EmbedChannelBuf.WriteString("\n")
				message.EmbedNameBuf.WriteString("\n")
				message.EmbedUptimeBuf.WriteString("\n")
			}
			needLineWrap = true

			fmt.Fprintf(&message.EmbedChannelBuf, "<#%s>", u.channelID)
			fmt.Fprintf(&message.EmbedNameBuf, "<@%s>", u.id)
			fmt.Fprintf(&message.EmbedUptimeBuf, "`%02d:%02d:%02d`", h, m, s)
		}

		if len(userList) == 0 {
			message.EmbedChannelBuf.WriteString("\u200B")
			message.EmbedNameBuf.WriteString("\u200B")
			message.EmbedUptimeBuf.WriteString("\u200B")
		}

		embed.fieldChannel.Value = message.EmbedChannelBuf.String()
		embed.fieldName.Value = message.EmbedNameBuf.String()
		embed.fieldUptime.Value = message.EmbedUptimeBuf.String()

		if message.EmbedIDList[embedIndex] == "" {
			embedIndex := embedIndex

			w.Add(1)
			go func() {
				defer w.Done()

				msgSend := discordgo.MessageSend{
					AllowedMentions: &allowedMentions,
					Embed:           &embed.StatEmbed,
				}

				dsm, err := discordSession.ChannelMessageSendComplex(config.TextChannelID, &msgSend)
				if err != nil {
					sentry.CaptureException(err)
				}

				message.EmbedIDList[embedIndex] = dsm.ID
			}()
		} else {
			embedIndex := embedIndex

			w.Add(1)
			go func() {
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
			}()
		}
	}

	w.Wait()
}
