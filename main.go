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
	"mvdan.cc/xurls/v2"
	"net/http"
	"net/url"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

var auddToken = ""
var ravenDSN = "https://...@sentry.io/..."

var triggers = []string{"what's the song", "what is the song", "what's this song", "what is this song",
	"what song is playing", "what the song is playing", "recognizesong", "auddbot"}

var replySettings = map[string]bool{
	"mentionLinks": true,
	"commentLinks": false,
	"postLinks": false,
	"mentionReplyAlways": true,
	"commentReplyAlways": false,
	"postReplyAlways": false,
}

var commentsCounter uint64 = 0
var postsCounter uint64 = 0

var markdownRegex = regexp.MustCompile(`\[[^][]+]\((https?://[^()]+)\)`)
var rxStrict = xurls.Strict()

type rComment struct {
	r1 *reddit.Comment
	r2 *reddit2.Comment
}

func linksFromBody(body string) [][]string{
	results := markdownRegex.FindAllStringSubmatch(body, -1)
	//if len(results) == 0 {
		plaintextUrls := rxStrict.FindAllString(body, -1)
		for i := range plaintextUrls {
			plaintextUrls[i] = strings.ReplaceAll(plaintextUrls[i], "\\", "")
			results = append(results, []string{plaintextUrls[i], plaintextUrls[i]})
		}
	//}
	return results
}

func (r *auddBot) GetLinkFromComment(mention *reddit2.Message, commentsTree []rComment, post *reddit.Post) (string, error) {
	// Check:
	// - first for the links in the comment
	// - then for the links in the comment to which it was a reply
	// - then for the links in the post
	var resultUrl string
	if post != nil {
		resultUrl = post.URL
	}
	if len(commentsTree) > 0 {
		if commentsTree[0].r2 != nil {
			if resultUrl == "" {
				resultUrl = commentsTree[0].r2.LinkURL
			}
			mention = commentToMessage(commentsTree[0].r2)
			if len(commentsTree) > 1 {
				commentsTree = commentsTree[1:]
			} else {
				commentsTree = make([]rComment, 0)
			}
		}
	} else {
		if mention == nil {
			if post == nil {
				return "", fmt.Errorf("mention, commentsTree, and post are all nil")
			}
			mention = &reddit2.Message{
				Context: post.Permalink,
				Body: post.Body,
			}
		}
	}
	if resultUrl == "" {
		j, _ := json.Marshal(post)
		fmt.Printf("Got a post that's not a link or an empty post (https://www.reddit.com%s, %s)\n", mention.Context, string(j))
		//return "", fmt.Errorf("got a post without any URL (%s)", string(j))
		resultUrl = "https://www.reddit.com"+ mention.Context
	}
	if strings.Contains(resultUrl, "reddit.com/") || resultUrl == "" {
		if post != nil {
			if strings.Contains(post.Body, "https://reddit.com/link/"+post.ID+"/video/") {
				s := strings.Split(post.Body, "https://reddit.com/link/"+post.ID+"/video/")
				s = strings.Split(s[1], "/")
				resultUrl = "https://v.redd.it/" + s[0] + "/"
			}
		}
	}
	// Use links:
	results := linksFromBody(mention.Body) // from the comment
	if len(commentsTree) > 0 {
		if commentsTree[0].r1 != nil {
			results = append(results, linksFromBody(commentsTree[0].r1.Body)...) // from the comment it's a reply to
		} else {
			capture(fmt.Errorf("commentTree shouldn't contain an r2 entry at this point! %v", commentsTree))
		}
	}
	if !strings.Contains(resultUrl, "reddit.com/") && resultUrl != "" {
		results = append(results, []string{resultUrl, resultUrl}) // that's the post link
	}
	if post != nil {
		results = append(results, linksFromBody(post.Body)...) // from the post body
	}
	if len(results) != 0 {
		// And now get the first link that's OK
		fmt.Println("Parsed from the text:", results)
		for u := range results {
			if strings.HasPrefix(results[u][1], "/") {
				continue
			}
			if strings.HasPrefix(results[u][1], "https://www.reddit.com/") {
				continue
			}
			resultUrl = results[u][1]
			break
		}
	}

	if strings.Contains(resultUrl, "reddit.com/") {
		jsonUrl := resultUrl + ".json"
		// ToDo: fix for when it's a link to a comment
		// e.g., https://www.reddit.com/r/NameThatSong/comments/m5o9ck/intense_and_hype_high_bpm_probably_repetitive/gr0y4fx/?context=3
		resp, err := http.Get(jsonUrl)
		defer resp.Body.Close()
		if !capture(err) {
			var page RedditPageJSON
			body, err := ioutil.ReadAll(resp.Body)
			capture(err)
			//fmt.Println(string(body))
			fmt.Println("Getting", jsonUrl)
			err = json.Unmarshal(body, &page)
			capture(err)
			if len(page) > 0 {
				if len(page[0].Data.Children) > 0 {
					if strings.Contains(page[0].Data.Children[0].Data.URL, "v.redd.it") {
						resultUrl = page[0].Data.Children[0].Data.URL
					} else {
						if len(page[0].Data.Children[0].Data.MediaMetadata) > 0 {
							for s := range page[0].Data.Children[0].Data.MediaMetadata {
								resultUrl = "https://v.redd.it/" + s + "/"
							}
						}
					}
				}
			}
		}
	}
	if strings.Contains(resultUrl, "vocaroo.com/") || strings.Contains(resultUrl, "https://voca.ro/") {
		resultUrl = "https://media1.vocaroo.com/mp3/" + strings.Split(resultUrl, "/")[3]
	}
	if strings.Contains(resultUrl, "https://v.redd.it/") {
		resultUrl += "/DASH_audio.mp4"
	}
	if strings.Contains(resultUrl, "https://reddit.com/link/") {
		capture(fmt.Errorf("got an unparsed URL, %s", resultUrl))
	}
	// ToDo: parse the the body and check if there's a timestamp; add it to the t URL parameter
	// (maybe, only when there's no t parameter in the url?)
	return resultUrl, nil
}

func (r *auddBot) GetVideoLink(mention *reddit2.Message, comment *reddit2.Comment) (string, error) {
	var post *reddit.Post
	if mention == nil && comment == nil {
		return "", fmt.Errorf("empty mention and comment")
	}
	var parentId string
	commentsTree := make([]rComment, 0)
	if mention != nil {
		parentId = mention.ParentID
	} else {
		parentId = comment.ParentID
		/*_, comments, _, _, err := r.client.Listings.Get(context.Background(), comment.Name)
		if capture(err) {
			return "", err
		}
		if len(comments) == 0 {
			return "", fmt.Errorf("can't get the comment")
		}
		if comments[0] == nil {
			return "", fmt.Errorf("can't get the comment")
		}*/
		commentsTree = append(commentsTree, rComment{r2: comment})
	}
	for ; parentId != ""; {
		posts, comments, _, _, err := r.client.Listings.Get(context.Background(), parentId)
		//r.bot.Listing(parentId, )
		j, _ := json.Marshal([]interface{}{posts, comments})
		fmt.Println(string(j))
		parentId = ""
		if err != nil {
			return "", err
		}
		if len(posts) > 0 {
			if posts[0] != nil {
				post = posts[0]
				break
			}
		}
		if len(comments) > 0 {
			if comments[0] != nil {
				commentsTree = append(commentsTree, rComment{r1: comments[0]})
				parentId = comments[0].ParentID
			}
		}
	}
	// ToDo: find out why Reddit sometimes doesn't give us the post and fix
	return r.GetLinkFromComment(mention, commentsTree, post)
}

func GetSkipFromLink(resultUrl string) int {
	skip := -1
	u, err := url.Parse(resultUrl)
	if err == nil {
		if t := u.Query().Get("t"); t != "" {
			t = strings.ReplaceAll(t, "s", "")
			tInt := 0
			if strings.Contains(t, "m") {
				s := strings.Split(t, "m")
				tsInt, _ := strconv.Atoi(s[1])
				tInt += tsInt
				if strings.Contains(s[0], "h") {
					s := strings.Split(s[0], "h")
					if tmInt, err := strconv.Atoi(s[1]); !capture(err) {
						tInt += tmInt * 60
					}
					if thInt, err := strconv.Atoi(s[0]); !capture(err) {
						tInt += thInt * 60 * 60
					}
				} else {
					if tmInt, err := strconv.Atoi(s[0]); !capture(err) {
						tInt += tmInt * 60
					}
				}
			} else {
				if tsInt, err := strconv.Atoi(t); !capture(err) {
					tInt = tsInt
				}
			}
			skip -= tInt / 18
			fmt.Println("skip:", skip)
		}
	}
	return skip
}

func GetReply(result []audd.RecognitionEnterpriseResult, withLinks bool) string {
	links := map[string]bool{}
	texts := make([]string, 0)
	for _, results := range result {
		if len(results.Songs) == 0 {
			capture(fmt.Errorf("enterprise response has a result without any songs"))
		}
		for _, song := range results.Songs {
			if song.SongLink != "" {
				if _, exists := links[song.SongLink]; exists { // making sure this song isn't a duplicate
					continue
				}
				links[song.SongLink] = true
			}
			album := ""
			label := ""
			if song.Title != song.Album && song.Album != "" {
				album = "Album: ^(" + song.Album + "). "
			}
			if song.Artist != song.Label && song.Label != "Self-released"  && song.Label != "" {
				label = " by ^(" + song.Label + ")"
			}
			score := strconv.Itoa(song.Score) + "%"
			text := fmt.Sprintf("[**%s** by %s](%s) (%s; confidence: ^(%s)))\n\n%sReleased on ^(%s)%s.",
				song.Title, song.Artist, song.SongLink, song.Timecode, score, album, song.ReleaseDate, label)
			if !withLinks {
				text = fmt.Sprintf("**%s** by %s (%s; confidence: ^(%s)))\n\n%sReleased on ^(%s)%s.",
					song.Title, song.Artist, song.Timecode, score, album, song.ReleaseDate, label)
			}
			texts = append(texts, text)
		}
	}
	if len(texts) == 0 {
		return ""
	}
	response := texts[0]
	if len(texts) > 1 {
		response = "Recognized multiple songs:"
		for i, text := range texts {
			response += fmt.Sprintf("\n\n%d. %s", i+1, text)
		}
	}
	return response
}

func (r *auddBot) HandleQuery(mention *reddit2.Message, comment *reddit2.Comment, post *reddit2.Post) {
	var resultUrl, t, parentID, body string
	var err error
	if mention != nil {
		t, parentID, body = "mention", mention.Name, mention.Body
	}
	if comment != nil {
		t, parentID, body = "comment", comment.Name, comment.Body
	}
	if post != nil {
		t, parentID, body = "post", post.Name, post.SelfText
		resultUrl, err = r.GetLinkFromComment(nil, nil,
			&reddit.Post{ID: post.ID, URL: post.URL, Permalink: post.Permalink, Body: post.SelfText})
	} else {
		resultUrl, err = r.GetVideoLink(mention, comment)
	}
	if capture(err) {
		return
	}
	skip := GetSkipFromLink(resultUrl)
	fmt.Println(resultUrl)
	limit := 2
	if strings.Contains(resultUrl, "v.redd.it") {
		limit = 3
	}
	result, err := r.audd.RecognizeLongAudio(resultUrl,
		map[string]string{"accurate_offsets":"true", "skip":strconv.Itoa(skip), "limit":strconv.Itoa(limit)})
	if capture(err) {
		return
	}
	links := []string{
		"[GitHub](https://github.com/AudDMusic/RedditBot) " +
			"[^(new issue)](https://github.com/AudDMusic/RedditBot/issues/new)",
		"[Donate](https://www.patreon.com/audd)",
		"[Feedback](https://www.reddit.com/message/compose?to=Mihonarium&subject=Music20recognition)",
	}
	donateLink := 1
	if len(result) == 0 {
		if !replySettings[t + "ReplyAlways"] {
			return
		}
		links = append(links[:donateLink], links[donateLink:]...)
	}
	footer := "\n\n" + strings.Join(links, " | ")
	withLinks := replySettings[t + "Links"] &&
		!strings.Contains(body, "without links") && !strings.Contains(body, "/wl")
	response := GetReply(result, withLinks)
	if response == "" {
		timestamp := skip*-1 - 1
		timestamp *= 18
		response = fmt.Sprintf("Sorry, I couldn't recognize the song."+
			"\n\nI tried to identify music from the [link](%s) at %d-%d seconds.",
			resultUrl, timestamp, timestamp+limit*18)
		if strings.Contains(resultUrl, "https://www.reddit.com/") {
			response = "Sorry, I couldn't get the video URL from the post or your comment."
		}
	}
	if withLinks {
		response += footer
	}
	if t == "comment" {
		fmt.Println("testing", t, response)
		return
	}
	fmt.Println(response)
	err = r.bot.Reply(parentID, response)
	capture(err)
}

func (r *auddBot) Mention(p *reddit2.Message) error {
	// ToDo: find out why we don't get all the mentions
	// (e.g., https://www.reddit.com/r/NameThatSong/comments/m5l84h/chirptuneish_something_that_would_be_in_mo3/gr0jpc2/?utm_source=reddit&utm_medium=web2x&context=3)
	j, _ := json.Marshal(p)
	fmt.Println("Got a mention", string(j))
	if !p.New {
		return nil
	}
	_, err := r.client.Message.Read(context.Background(), p.Name)
	capture(err)
	r.HandleQuery(p, nil, nil)
	return nil
}

func (r *auddBot) Comment(p *reddit2.Comment) error {
	//fmt.Print("c") // why? to test the amount of new comments on Reddit!
	atomic.AddUint64(&commentsCounter, 1)
	//return nil
	compare := strings.ToLower(p.Body)
	trigger := false
	for i := range triggers {
		if strings.Contains(compare, triggers[i]) {
			trigger = true
			break
		}
	}
	if !trigger {
		return nil
	}
	j, _ := json.Marshal(p)
	fmt.Println("Got a comment", string(j))
	r.HandleQuery(nil, p, nil)
	return nil
}

func (r *auddBot) Post(p *reddit2.Post) error {
	atomic.AddUint64(&postsCounter, 1)
	compare := strings.ToLower(p.SelfText)
	trigger := false
	for i := range triggers {
		if strings.Contains(compare, triggers[i]) {
			trigger = true
			break
		}
	}
	compare = strings.ToLower(p.Title)
	for i := range triggers {
		if strings.Contains(compare, triggers[i]) {
			trigger = true
			break
		}
	}
	if !trigger {
		return nil
	}
	j, _ := json.Marshal(p)
	fmt.Println("Got a post", string(j))
	r.HandleQuery(nil, nil, p)
	return nil
}

type auddBot struct {
	bot reddit2.Bot
	client *reddit.Client
	audd *audd.Client
}

func getCredentials(filename string) *auddBot {
	buf, err := ioutil.ReadFile(filename)
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

	//client.Stream.Posts("")

	bot, err := reddit2.NewBotFromAgentFile(filename, 0)
	if capture(err) {
		panic(err)
	}
	handler := &auddBot{bot: bot, client: client, audd: audd.NewClient(auddToken)}
	handler.audd.SetEndpoint(audd.EnterpriseAPIEndpoint)
	return handler
}

func main() {
	go func() {
		for {
			fmt.Println("Comments:", atomic.LoadUint64(&commentsCounter), "posts:", atomic.LoadUint64(&postsCounter))
			<- time.NewTicker(time.Minute).C
		}
	}()
	//client.Stream.Posts("")

	handler := getCredentials("RecognizeSong.agent")
	// See https://docs.audd.io/enterprise
	cfg := graw.Config{Mentions: true}
	_, wait, err := graw.Run(handler, handler.bot, cfg)
	if capture(err) {
		panic(err)
	}

	handler2 := getCredentials("auddbot.agent")
	cfg2 := graw.Config{SubredditComments: []string{"all"}, Subreddits: []string{"all"}, Mentions: true}
	_, wait2, err := graw.Run(handler2, handler2.bot, cfg2)
	capture(err)
	fmt.Println("started")
	fmt.Println("graw run failed: ", wait(), wait2())
}

func init() {
	err := raven.SetDSN(ravenDSN)
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

func commentToMessage(comment *reddit2.Comment) *reddit2.Message {
	if comment == nil {
		return nil
	}
	return &reddit2.Message{
		ID:               comment.ID,
		Name:             comment.Name,
		CreatedUTC:       comment.CreatedUTC,
		Author:           comment.Author,
		Body:             comment.Body,
		BodyHTML:         comment.BodyHTML,
		Context:          comment.Permalink,
		ParentID:         comment.ParentID,
		Subreddit:        comment.Subreddit,
		WasComment:       true,
	}
}

type RedditPageJSON []struct {
	Data struct {
		Children []struct {
			Data struct {
				MediaMetadata         map[string]struct {
						Status  string `json:"status"`
						E       string `json:"e"`
						DashURL string `json:"dashUrl"`
						X       int    `json:"x"`
						Y       int    `json:"y"`
						HlsURL  string `json:"hlsUrl"`
						ID      string `json:"id"`
						IsGif   bool   `json:"isGif"`
					}								   `json:"media_metadata"`
				URL                      string        `json:"url"`
			} `json:"data"`
		} `json:"children"`
		After  interface{} `json:"after"`
		Before interface{} `json:"before"`
	} `json:"data"`
}
