package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/AudDMusic/audd-go"
	"github.com/getsentry/raven-go"
	"github.com/golang/protobuf/proto"
	"github.com/turnage/graw"
	reddit2 "github.com/turnage/graw/reddit"
	"github.com/turnage/redditproto"
	"github.com/vartanbeno/go-reddit/v2/reddit"
	"io/ioutil"
	"runtime"
	"strings"
)

func (r *auddBot) Mention(p *reddit2.Message) error {
	j, _ := json.Marshal(p)
	fmt.Println(string(j))
	if !p.New {
		return nil
	}
	_, err := r.client.Message.Read(context.Background(), p.Name)
	capture(err)
	resultUrl := ""
	for posts, parentId := []*reddit.Post{}, p.ParentID; len(posts) == 0 && parentId != ""; {
		posts, comments, _, response, err := r.client.Listings.Get(context.Background(), parentId)
		parentId = ""
		if capture(err) {
			return nil
		}
		if len(posts) > 0 {
			if posts[0] != nil {
				resultUrl = posts[0].URL
				break
			}
		}
		if len(comments) > 0 {
			if comments[0] != nil {
				parentId = comments[0].ParentID
			}
		}
		fmt.Println(response.Rate.Remaining)
	}
	if strings.Contains(resultUrl, "https://v.redd.it/") {
		resultUrl += "/DASH_audio.mp4"
	}
	result, err := r.audd.RecognizeLongAudio(resultUrl,
		map[string]string{"accurate_offsets":"true", "skip":"-1", "limit":"3"})
	if capture(err) {
		return nil
	}
	if len(result) > 0 {
		if len(result[0].Songs) == 0 {
			capture(fmt.Errorf("enterprise response has a result without any songs"))
		}
		song := result[0].Songs[0]
		album := ""
		if song.Title != song.Album {
			album = "Album: **" + song.Album + "**. "
		}
		text := fmt.Sprintf("[**%s** by %s](%s) (%s)\n\n%sReleased on %s by %s.\n\n" +
			"[**Github**](https://github.com/AudDMusic/RedditBot) | " +
			"[**Contact**](https://www.reddit.com/message/compose?to=Mihonarium&subject=Music20recognition) | " +
			"[**Support on Patreon**](https://www.patreon.com/audd)",
			song.Title, song.Artist, song.SongLink, song.Timecode, album, song.ReleaseDate, song.Label)
		fmt.Println(text)
		err = r.bot.Reply(p.Name, text)
		capture(err)
	}
	return nil
}

type auddBot struct {
	bot reddit2.Bot
	client *reddit.Client
	audd *audd.Client
}

func main() {
	reddit.DefaultClient()

	buf, err := ioutil.ReadFile("auddbot.agent")
	if capture(err) {
		panic(err)
	}

	agent := &redditproto.UserAgent{}
	err = proto.UnmarshalText(bytes.NewBuffer(buf).String(), agent)
	if capture(err) {
		panic(err)
	}

	credentials := reddit.Credentials{ID: *agent.ClientId, Secret: *agent.ClientSecret,
		Username: *agent.Username, Password: *agent.Password}
	client, err := reddit.NewClient(credentials, reddit.WithUserAgent(*agent.UserAgent))
	if capture(err) {
		panic(err)
	}

	bot, err := reddit2.NewBotFromAgentFile("auddbot.agent", 0)
	if capture(err) {
		panic(err)
	}

	cfg := graw.Config{Mentions: true}
	handler := &auddBot{bot: bot, client: client, audd: audd.NewClient("test")}
	handler.audd.SetEndpoint(audd.EnterpriseAPIEndpoint)
	_, wait, err := graw.Run(handler, bot, cfg)
	if capture(err) {
		panic(err)
	}
	fmt.Println("graw run failed: ", wait())
}

func init() {
	err := raven.SetDSN("")
	if err != nil {
		panic(err)
	}
}

func capture(err error) bool {
	if err == nil {
		return false
	}
	_, file, no, ok := runtime.Caller(1)
	if ok {
		err = fmt.Errorf("%v from %s#%d", err, file, no)
	}
	packet := raven.NewPacket(err.Error(), raven.NewException(err, raven.GetOrNewStacktrace(err, 1, 3, nil)))
	go raven.Capture(packet, nil)
	fmt.Println(err.Error())
	return true
}
