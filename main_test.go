package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/AudDMusic/audd-go"
	"github.com/golang/protobuf/proto"
	reddit2 "github.com/turnage/graw/reddit"
	"github.com/turnage/redditproto"
	"github.com/vartanbeno/go-reddit/v2/reddit"
	"io/ioutil"
	"testing"
)

func TestGetVideoLink(t *testing.T)  {
	buf, err := ioutil.ReadFile("auddbot.agent")
	if err != nil {
		t.Fatal(err)
	}
	agent := &redditproto.UserAgent{}
	err = proto.UnmarshalText(bytes.NewBuffer(buf).String(), agent)
	if err != nil {
		t.Fatal(err)
	}
	credentials := reddit.Credentials{ID: *agent.ClientId, Secret: *agent.ClientSecret,
		Username: *agent.Username, Password: *agent.Password}
	client, err := reddit.NewClient(credentials, reddit.WithUserAgent(*agent.UserAgent))
	if err != nil {
		t.Fatal(err)
	}
	bot, err := reddit2.NewBotFromAgentFile("auddbot.agent", 0)
	if err != nil {
		t.Fatal(err)
	}
	r := &auddBot{bot: bot, client: client, audd: audd.NewClient("test")}

	mentionJSONs := []string{
		`{"ID":"gqx2yi2","Name":"t1_gqx2yi2","CreatedUTC":1615744156,"Author":"Mihonarium","Subject":"username mention","Body":"u/RecognizeSong with links","BodyHTML":"\u003c!-- SC_OFF --\u003e\u003cdiv class=\"md\"\u003e\u003cp\u003e\u003ca href=\"/u/RecognizeSong\"\u003eu/RecognizeSong\u003c/a\u003e with links\u003c/p\u003e\n\u003c/div\u003e\u003c!-- SC_ON --\u003e","Context":"/r/NameThatSong/comments/m4wtq3/can_someone_help_me_identify_the_name_of_the/gqx2yi2/?context=3","FirstMessageName":"","Likes":false,"LinkTitle":"Can someone help me identify the name of the music used in this Tiktok?","New":true,"ParentID":"t3_m4wtq3","Subreddit":"NameThatSong","WasComment":true}`,
	}
	links := []string{
		"https://v.redd.it/rvleasnkf0n61/",
	}
	for i := range mentionJSONs {
		var mention reddit2.Message
		err := json.Unmarshal([]byte(mentionJSONs[i]), &mention)
		if err != nil {
			panic(err)
		}
		name := mention.Context
		t.Run(name, func(t *testing.T) {
			Url, err := r.GetVideoLink(&mention)
			ExpectedUrl := links[i]
			if err != nil && ExpectedUrl != "" {
				t.Errorf("Got unexpected error %s, want: %s", err.Error(), ExpectedUrl)
			}
			if Url != ExpectedUrl && Url != ExpectedUrl+ "/DASH_audio.mp4" {
				t.Errorf("Video URL was incorrect, got: %s, want: %s", Url, ExpectedUrl)
			}
		})
	}
}

func TestGetSkipFromLink(t *testing.T) {
	cases := []struct{
		VideoUrl string
		skip int
	}{
		{"https://www.twitch.tv/videos/577831096", -1},
		{"https://www.twitch.tv/videos/949398316?t=00h16m19s", -55},
		{"https://youtu.be/aCcVpANtJGE", -1},
		{"https://youtu.be/aCcVpANtJGE?t=test", -1},
		{"https://youtu.be/aCcVpANtJGE?t=53", -3},
		{"https://www.youtube.com/watch?v=aCcVpANtJGE&t=53s", -3},
	}
	for i := range cases {
		name := fmt.Sprintf("%s, %d", cases[i].VideoUrl, cases[i].skip)
		t.Run(name, func(t *testing.T) {
			skip := GetSkipFromLink(cases[i].VideoUrl)
			ExpectedSkip := cases[i].skip
			if skip != ExpectedSkip {
				t.Errorf("Skip was incorrect, got: %d, want: %d", skip, ExpectedSkip)
			}
		})

	}
}
