package main

import (
	"bytes"
	"container/ring"
	"encoding/json"
	"fmt"
	"github.com/AudDMusic/audd-go"
	"github.com/getsentry/raven-go"
	"github.com/golang/protobuf/proto"
	"github.com/surajJha/go-profanity-hindi"
	"github.com/ttgmpsn/mira"
	"github.com/ttgmpsn/mira/models"
	"github.com/turnage/graw"
	reddit1 "github.com/turnage/graw/reddit"
	"github.com/turnage/redditproto"
	"io/ioutil"
	"math"
	"mvdan.cc/xurls/v2"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

//ToDo: move to the config
var ignoreSubreddits = []string{
	// subreddits bots should generally avoid
	"suicidewatch",
	"depression",

	// subreddits with account age thresholds
	"wallstreetbets", // 45 days
	"dogelore",       // 21 days
	"fightporn",      // 14 days
	"Hololive",      // unknown

	// subreddits that banned the bot
	"tiktokthots",        // banned
	"actuallesbians",     // banned
	"GraceBoorBoutineLA", // banned
	"Minecraft",          // banned
	"anime",              // banned

	// subreddits I decided the bot should avoid
	"MAAU", // homophobic slurs about the bot's avatar (and I didn't know that's a thing on Reddit!)
}

type BotConfig struct {
	AudDToken     string      `json:"AudDToken"`
	Triggers      []string    `json:"Triggers"`
	AntiTriggers  []string    `json:"AudDTAntiTriggers"`
	ReplySettings map[string]ReplyConfig `json:"ReplySettings"`
	RavenDSN      string      `json:"RavenDSN"`
}

type ReplyConfig struct {
	SendLinks bool `json:"SendLinks"`
	ReplyAlways bool `json:"ReplyAlways"`
}
var commentsCounter int64 = 0
var postsCounter int64 = 0

var markdownRegex = regexp.MustCompile(`\[[^][]+]\((https?://[^()]+)\)`)
var rxStrict = xurls.Strict()

func linksFromBody(body string) [][]string {
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

func getJSON(URL string, v interface{}) error {
	resp, err := http.Get(URL)
	if err != nil {
		return err
	}
	defer captureFunc(resp.Body.Close)
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	err = json.Unmarshal(body, v)
	return err
}

func (r *auddBot) GetLinkFromComment(mention *reddit1.Message, commentsTree []*models.Comment, post *models.Post) (string, error) {
	// Check:
	// - first for the links in the comment
	// - then for the links in the comment to which it was a reply
	// - then for the links in the post
	var resultUrl string
	if post != nil {
		resultUrl = post.URL
	}
	if strings.Contains(resultUrl, "reddit.com/rpan") {
		s := strings.Split(resultUrl, "/")
		jsonUrl := "https://strapi.reddit.com/videos/t3_" + s[len(s)-1]
		var page RedditStreamJSON
		err := getJSON(jsonUrl, &page)
		if err != nil {
			return "", err
		}
		resultUrl = page.Data.Stream.HlsURL
		if resultUrl == "" {
			return "", fmt.Errorf("got an empty URL of the live stream HLS")
		}
		return resultUrl, nil
	}
	if len(commentsTree) > 0 {
		if commentsTree[0] != nil {
			if resultUrl == "" {
				//resultUrl = commentsTree[0].r2.LinkURL
			}
			if mention == nil {
				mention = commentToMessage(commentsTree[0])
				if len(commentsTree) > 1 {
					commentsTree = commentsTree[1:]
				} else {
					commentsTree = make([]*models.Comment, 0)
				}
			}
		}
	} else {
		if mention == nil {
			if post == nil {
				return "", fmt.Errorf("mention, commentsTree, and post are all nil")
			}
			mention = &reddit1.Message{
				Context: post.Permalink,
				Body:    post.Selftext,
			}
		}
	}
	if resultUrl == "" {
		j, _ := json.Marshal(post)
		fmt.Printf("Got a post that's not a link or an empty post (https://www.reddit.com%s, %s)\n", mention.Context, string(j))
		//return "", fmt.Errorf("got a post without any URL (%s)", string(j))
		resultUrl = "https://www.reddit.com" + mention.Context
	}
	if strings.Contains(resultUrl, "reddit.com/") || resultUrl == "" {
		if post != nil {
			if strings.Contains(post.Selftext, "https://reddit.com/link/"+post.ID+"/video/") {
				s := strings.Split(post.Selftext, "https://reddit.com/link/"+post.ID+"/video/")
				s = strings.Split(s[1], "/")
				resultUrl = "https://v.redd.it/" + s[0] + "/"
			}
		}
	}
	// Use links:
	results := linksFromBody(mention.Body) // from the comment
	if len(commentsTree) > 0 {
		if commentsTree[0] != nil {
			results = append(results, linksFromBody(commentsTree[0].Body)...) // from the comment it's a reply to
		} else {
			capture(fmt.Errorf("commentTree shouldn't contain an r2 entry at this point! %v", commentsTree))
		}
	}
	if !strings.Contains(resultUrl, "reddit.com/") && resultUrl != "" {
		results = append(results, []string{resultUrl, resultUrl}) // that's the post link
	}
	if post != nil {
		results = append(results, linksFromBody(post.Selftext)...) // from the post body
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
		var page RedditPageJSON
		err := getJSON(jsonUrl, &page)
		if !capture(err) {
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
		s := strings.Split(resultUrl, "/")
		resultUrl = "https://media1.vocaroo.com/mp3/" + s[len(s)-1]
	}
	if strings.Contains(resultUrl, "https://v.redd.it/") {
		resultUrl += "/DASH_audio.mp4"
	}
	if strings.Contains(resultUrl, "https://reddit.com/link/") && strings.Contains(resultUrl, "/video/") {
		s := strings.TrimSuffix(resultUrl, "/")
		s = strings.ReplaceAll(s, "/player", "")
		s_ := strings.Split(s, "/")
		resultUrl = "https://v.redd.it/" + s_[len(s_)-1] + "/DASH_audio.mp4"
	}
	if strings.Contains(resultUrl, "https://reddit.com/link/") {
		capture(fmt.Errorf("got an unparsed URL, %s", resultUrl))
	}
	// ToDo: parse the the body and check if there's a timestamp; add it to the t URL parameter
	// (maybe, only when there's no t parameter in the url?)
	return resultUrl, nil
}

func (r *auddBot) GetVideoLink(mention *reddit1.Message, comment *models.Comment) (string, error) {
	var post *models.Post
	if mention == nil && comment == nil {
		return "", fmt.Errorf("empty mention and comment")
	}
	var parentId models.RedditID
	commentsTree := make([]*models.Comment, 0)
	if mention != nil {
		parentId = models.RedditID(mention.ParentID)
	} else {
		parentId = comment.ParentID
		commentsTree = append(commentsTree, comment)
	}
	for parentId != "" {
		//posts, comments, _, _, err := r.client.Listings.Get(context.Background(), parentId)
		parent, err := r.r.SubmissionInfoID(parentId)
		//r.bot.Listing(parentId, )
		j, _ := json.Marshal(parent)
		fmt.Printf("parent [%s]: %s\n", string(parentId), string(j))
		if err != nil && err.Error() != "no results" {
			return "", err
		}
		if err == nil {
			if p, ok := parent.(*models.Post); ok {
				post = p
				break
			}
			if c, ok := parent.(*models.Comment); ok {
				commentsTree = append(commentsTree, c)
				parentId = c.ParentID
			} else {
				return "", fmt.Errorf("got a result that's neither a post nor a comment, parent ID %s, %v",
					parentId, parent)
			}
		} else {
			parentId = ""
		}
	}
	return r.GetLinkFromComment(mention, commentsTree, post)
}

func GetSkipFromLink(resultUrl string) int {
	skip := -1
	if strings.HasSuffix(resultUrl, ".m3u8") {
		return skip
	}
	u, err := url.Parse(resultUrl)
	if err == nil {
		t := u.Query().Get("t")
		if t == "" {
			t = u.Query().Get("time_continue") // Thanks to mike-fmh for the idea of "start" and "time_continue"
			if t == "" {
				t = u.Query().Get("start")
			}
		}
		if t != "" {
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

func GetReply(result []audd.RecognitionEnterpriseResult, withLinks, full bool) string {
	if len(result) == 0 {
		return ""
	}
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
			score := strconv.Itoa(song.Score) + "%"
			text := fmt.Sprintf("[**%s** by %s](%s)",
				song.Title, song.Artist, song.SongLink)
			if !withLinks {
				text = fmt.Sprintf("**%s** by %s",
					song.Title, song.Artist)
			}
			if full {
				album := ""
				label := ""
				if song.Title != song.Album && song.Album != "" {
					album = "Album: `" + song.Album + "`. "
				}
				if song.Artist != song.Label && song.Label != "Self-released" && song.Label != "" {
					label = " by `" + song.Label + "`"
				}
				text += fmt.Sprintf(" (%s; matched: `%s`)\n\n%sReleased on `%s`%s.",
					song.Timecode, score, album, song.ReleaseDate, label)
			}
			texts = append(texts, text)
		}
	}
	if len(texts) == 0 {
		return ""
	}
	response := texts[0]
	if len(texts) > 1 {
		if full {
			response = "I got matches with these songs:"
		} else {
			response = ""
		}
		for i, text := range texts {
			response += fmt.Sprintf("\n\n%d. %s", i+1, text)
		}
	}
	response = profanity.MaskProfanity(response, "#")
	return response
}

type d struct {
	b bool
	u string
}

var avoidDuplicates = map[string]chan d{}
var avoidDuplicatesMu = &sync.Mutex{}

var myReplies = map[string]bool{}
var myRepliesMu = &sync.Mutex{}

func (r *auddBot) HandleQuery(mention *reddit1.Message, comment *models.Comment, post *models.Post) {
	var resultUrl, t, parentID, body string
	var err error
	if mention != nil {
		t, parentID, body = "mention", mention.Name, mention.Body
		fmt.Println("\n ! Processing the mention")
	}
	if comment != nil {
		t, parentID, body = "comment", string(comment.GetID()), comment.Body
	}
	if post != nil {
		t, parentID, body = "post", string(post.GetID()), post.Selftext
	}

	// Avoid handling of both the comment from r/all and the mention
	var previousUrl string
	var c chan d
	var exists bool
	avoidDuplicatesMu.Lock()
	if c, exists = avoidDuplicates[parentID]; exists {
		delete(avoidDuplicates, parentID)
		avoidDuplicatesMu.Unlock()
		results := <-c
		if results.b {
			fmt.Println("Ignored a duplicate")
			return
		}
		fmt.Println("Attempting to recognize the song again")
		c = make(chan d, 1)
		previousUrl = results.u
	} else {
		c = make(chan d, 1)
		avoidDuplicates[parentID] = c
		avoidDuplicatesMu.Unlock()
	}

	if post != nil {
		resultUrl, err = r.GetLinkFromComment(nil, nil, post)
	} else {
		resultUrl, err = r.GetVideoLink(mention, comment)
	}
	if capture(err) {
		return
	}

	skip := GetSkipFromLink(resultUrl)
	if resultUrl == previousUrl {
		fmt.Println("Got the same URL, skipping")
		return
	}
	fmt.Println(resultUrl)
	limit := 2
	if strings.Contains(resultUrl, "v.redd.it") {
		limit = 3
	}
	body = strings.ToLower(body)
	rs := strings.Contains(body, "u/recognizesong")
	summoned := rs || strings.Contains(body, "u/auddbot")
	if strings.HasSuffix(resultUrl, ".m3u8") {
		fmt.Println("\nGot a livestream", resultUrl)
		if summoned {
			reply := "I'll listen to the next 18 seconds of the stream and try to identify the song"
			if rs {
				go r.r.ReplyWithID(parentID, reply)

			} else {
				go r.r2.ReplyWithID(parentID, reply)
			}
		}
		limit = 1
	}
	withLinks := (summoned || r.config.ReplySettings[t].SendLinks) &&
		!strings.Contains(body, "without links") && !strings.Contains(body, "/wl")
	result, err := r.audd.RecognizeLongAudio(resultUrl,
		map[string]string{"accurate_offsets": "true", "skip": strconv.Itoa(skip), "limit": strconv.Itoa(limit)})
	response := GetReply(result, withLinks, true)
	if err != nil {
		if v, ok := err.(*audd.Error); ok {
			if v.ErrorCode == 501 {
				response = fmt.Sprintf("Sorry, I couldn't get any audio from the [link](%s)", resultUrl)
			}
		}
		if response == "" {
			capture(err)
			c <- d{false, resultUrl}
			return
		}
	}
	footerLinks := []string{
		"*I am a bot and this action was performed automatically*",
		"[GitHub](https://github.com/AudDMusic/RedditBot) " +
			"[^(new issue)](https://github.com/AudDMusic/RedditBot/issues/new)",
		"[Donate](https://www.patreon.com/audd)",
		"[Feedback](/message/compose?to=Mihonarium&subject=Music%20recognition)",
	}
	donateLink := 2
	if len(result) == 0 {
		if exists {
			fmt.Println("Couldn't recognize in a duplicate")
			return
		}
		if strings.Contains(resultUrl, "https://www.reddit.com/") {
			c <- d{true, resultUrl}
		} else {
			c <- d{false, resultUrl}
		}
		if !r.config.ReplySettings[t].ReplyAlways && !summoned {
			fmt.Println("No result")
			return
		}
	} else {
		c <- d{true, resultUrl}
	}
	//if len(result) == 0 || !summoned {
	if len(result) == 0 || !summoned {
		footerLinks = append(footerLinks[:donateLink], footerLinks[donateLink+1:]...)
	}
	footer := "\n\n" + strings.Join(footerLinks, " | ")

	if strings.HasSuffix(resultUrl, ".m3u8") {
		fmt.Println("\nStream results:", result)
	}
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
	fmt.Println(response)
	var cr *models.CommentActionResponse
	if rs {
		cr, err = r.r.ReplyWithID(parentID, response)
	} else {
		cr, err = r.r2.ReplyWithID(parentID, response)
	}
	if !capture(err) {
		if len(cr.JSON.Data.Things) > 0 {
			sentID := string(cr.JSON.Data.Things[0].Data.GetID())
			myRepliesMu.Lock()
			myReplies[sentID] = summoned
			myRepliesMu.Unlock()
			if !withLinks {
				response = "Links to the streaming platforms:\n\n"
				response += GetReply(result, true, false)
				response += footer
				if rs {
					cr, err = r.r.ReplyWithID(sentID, response)
				} else {
					cr, err = r.r2.ReplyWithID(sentID, response)
				}
			}
		}
	}
}

func (r *auddBot) Mention(p *reddit1.Message) error {
	// Note: it looks like we don't get all the mentions through this
	// In particular, we don't get mentions in replies to our comments
	//j, _ := json.Marshal(p)
	//fmt.Println("\n😻 Got a mention", string(j))
	fmt.Println("\n😻 Got a mention", p.Body)
	if !p.New {
		fmt.Println("Not a new mention")
		return nil
	}
	compare := getBodyToCompare(p.Body)
	for i := range r.config.AntiTriggers {
		if strings.Contains(compare, r.config.AntiTriggers[i]) {
			fmt.Println("Got an anti-trigger", p.Body)
			return nil
		}
	}
	go func() {
		capture(r.r.ReadMessage(p.Name))
		capture(r.r.ReadMessage(p.ID))
	}()
	r.HandleQuery(p, nil, nil)
	return nil
}
func replaceSlice(s, new string, oldStrings ...string) string {
	for _, old := range oldStrings {
		s = strings.ReplaceAll(s, old, new)
	}
	return s
}
func getBodyToCompare(body string) string {
	return strings.ToLower(replaceSlice(body, "", "'", "’", "`"))
}

func distance(s, sub1, sub2 string) (int, bool) {
	i1 := strings.Index(s, sub1)
	i2 := strings.Index(s, sub2)
	if i1 == -1 || i2 == -1 {
		return 0, false
	}
	return i2 - i1, true
}

func minDistance(s, sub1 string, sub2 ...string) int {
	if !strings.Contains(s, sub1) {
		return 0
	}

	min := math.MaxInt16
	for i := range sub2 {
		d, e := distance(s, sub1, sub2[i])
		if e && d < min && d > 0 {
			min = d
		}
	}
	if min == math.MaxInt16 {
		min = 0
	}
	return min
}

func (r *auddBot) Comment(p *models.Comment) {
	//fmt.Print("c") // why? to test the amount of new comments on Reddit!
	atomic.AddInt64(&commentsCounter, 1)
	//return nil
	compare := getBodyToCompare(p.Body)
	trigger := false
	for i := range r.config.Triggers {
		if strings.Contains(compare, r.config.Triggers[i]) {
			trigger = true
			break
		}
	}
	// ToDo: move to config
	if strings.Contains(compare, "bad bot") || strings.Contains(compare, "damn bot") ||
		strings.Contains(compare, "stupid bot") {
		myRepliesMu.Lock()
		summoned, exists := myReplies[string(p.ParentID)]
		myRepliesMu.Unlock()
		if exists && !summoned {
			target := mira.RedditOauth + "/api/del"
			_, err := r.r2.MiraRequest("POST", target, map[string]string{
				"id":       string(p.ParentID),
				"api_type": "json",
			})
			fmt.Println("got a bad bot comment", "https://reddit.com"+p.Permalink)
			capture(err)
		}
	}
	if !trigger {
		d := minDistance(compare, "what", "song", "music", "track")
		if len(compare) < 100 && d != 0 && d < 10 {
			fmt.Println("Skipping comment:", compare, "https://reddit.com"+p.Permalink)
		}
		return
	}
	for i := range ignoreSubreddits {
		if p.Subreddit == ignoreSubreddits[i] {
			fmt.Println("Ignoring a comment from", p.Subreddit, "https://reddit.com"+p.Permalink)
			return
		}
	}
	for i := range r.config.AntiTriggers {
		if strings.Contains(compare, r.config.AntiTriggers[i]) {
			fmt.Println("Got an anti-trigger", p.Body, "https://reddit.com"+p.Permalink)
			return
		}
	}
	//j, _ := json.Marshal(p)
	fmt.Println("\n😻 Got a comment", "https://reddit.com"+p.Permalink, p.Body)
	r.HandleQuery(nil, p, nil)
	return
}

func (r *auddBot) Post(p *models.Post) {
	atomic.AddInt64(&postsCounter, 1)
	compare := getBodyToCompare(p.Selftext)
	trigger := false
	for i := range r.config.Triggers {
		if strings.Contains(compare, r.config.Triggers[i]) {
			trigger = true
			break
		}
	}
	compare = strings.ToLower(p.Title)
	for i := range r.config.Triggers {
		if strings.Contains(compare, r.config.Triggers[i]) {
			trigger = true
			break
		}
	}
	if !trigger {
		return
	}
	j, _ := json.Marshal(p)
	fmt.Println("\n😻 Got a post", "https://reddit.com"+p.Permalink, string(j))
	r.HandleQuery(nil, nil, p)
	return
}

type auddBot struct {
	config BotConfig
	bot  reddit1.Bot
	audd *audd.Client
	r    *mira.Reddit
	r2   *mira.Reddit
}

func getReddit2Credentials(filename string) *mira.Reddit {
	buf, err := ioutil.ReadFile(filename)
	if capture(err) {
		return nil
	}
	agent := &redditproto.UserAgent{}
	err = proto.UnmarshalText(bytes.NewBuffer(buf).String(), agent)
	if capture(err) {
		return nil
	}
	r := mira.Init(mira.Credentials{
		ClientID:     *agent.ClientId,
		ClientSecret: *agent.ClientSecret,
		Username:     *agent.Username,
		Password:     *agent.Password,
		UserAgent:    *agent.UserAgent,
	})
	err = r.LoginAuth()
	if err != nil {
		return nil
	}
	if capture(err) {
		return nil
	}
	rMeObj, err := r.Me().Info()
	if err != nil {
		return nil
	}
	rMe, _ := rMeObj.(*models.Me)
	fmt.Printf("You are now logged in, /u/%s\n", rMe.Name)
	return r
}
func getReddit1Credentials(filename string) reddit1.Bot {
	buf, err := ioutil.ReadFile(filename)
	if capture(err) {
		return nil
	}
	agent := &redditproto.UserAgent{}
	err = proto.UnmarshalText(bytes.NewBuffer(buf).String(), agent)
	if capture(err) {
		return nil
	}
	bot, err := reddit1.NewBotFromAgentFile(filename, 0)
	if capture(err) {
		return nil
	}
	return bot
}

func loadConfig(file string) (*BotConfig, error) {
	var cfg BotConfig
	f, err := os.Open("config.json")
	if err != nil {
		return nil, err
	}
	j, err := ioutil.ReadAll(f)
	if err != nil {
		return nil, err
	}
	err = json.Unmarshal(j, &cfg)
	if err != nil {
		return nil, err
	}
	return &cfg, nil
}

func main() {
	// ToDo: watch the file and reload the config without losing comments and mentions
	// ToDo: get the config filename from a parameter
	// ToDo: move agents settings to the config
	cfg, err := loadConfig("config.json")
	if err != nil {
		panic(err)
	}
	if err := raven.SetDSN(cfg.RavenDSN); err != nil {
		panic(err)
	}

	for {
		handler := &auddBot{
			config: *cfg,
			bot:  getReddit1Credentials("RecognizeSong.agent"),
			audd: audd.NewClient(cfg.AudDToken),
			r:    getReddit2Credentials("RecognizeSong.agent"),
			r2:   getReddit2Credentials("auddbot.agent"),
		}
		if handler.bot == nil || handler.r == nil || handler.r2 == nil {
			time.Sleep(time.Second * 10)
			continue
		}
		stop := make(chan struct{}, 1)
		var grawStopChan = make(chan func(), 1)
		go func() {
			t := time.NewTicker(time.Minute)
			for {
				select {
				case <-stop:
					return
				default:
				}
				<-t.C
				newComments, newPosts := atomic.LoadInt64(&commentsCounter), atomic.LoadInt64(&postsCounter)
				fmt.Println("Comments:", newComments, "posts:", newPosts)
				atomic.AddInt64(&commentsCounter, -1*newComments)
				atomic.AddInt64(&postsCounter, -1*newPosts)
				if newComments == 0 {
					grawStop := <-grawStopChan
					grawStop()
					return
				}
			}
		}()
		handler.audd.SetEndpoint(audd.EnterpriseAPIEndpoint) // See https://docs.audd.io/enterprise
		/*_, err := handler.r.ListUnreadMessages()
		if capture(err) {
			panic(err)
		}
		for i := range m {
			//go handler.Comment(m[i])
			fmt.Println(m[i].Permalink)
			capture(handler.r.ReadMessage(m[i].ID))
		}*/
		cfg := graw.Config{Mentions: true}
		grawStop, wait, err := graw.Run(handler, handler.bot, cfg)
		if capture(err) {
			panic(err)
		}
		grawStopChan <- grawStop
		//ToDo: also get inbox messages to avoid not replying to mentions

		postsStream, err := streamSubredditPosts(handler.r, "all")
		if err != nil {
			panic(err)
		}
		commentsStream, err := streamSubredditComments(handler.r2, "all")
		if err != nil {
			panic(err)
		}
		go func() {
			var s models.Submission
			var p *models.Post
			for s = range postsStream.C {
				if s == nil {
					fmt.Println("Stream was closed")
					return
				}
				//go atomic.AddUint64(&postsCounter, 1)
				p = s.(*models.Post)
				go handler.Post(p)
			}
		}()
		go func() {
			var s models.Submission
			for s = range commentsStream.C {
				var c *models.Comment
				if s == nil {
					fmt.Println("Stream was closed")
					return
				}
				//go atomic.AddUint64(&commentsCounter, 1)
				c = s.(*models.Comment)
				go handler.Comment(c)
			}
		}()
		fmt.Println("started")
		fmt.Println("graw run failed: ", wait())
		go func() {
			commentsStream.Close <- struct{}{}
			postsStream.Close <- struct{}{}
			stop <- struct{}{}
		}()
	}

}

func streamSubredditPosts(c *mira.Reddit, name string) (*mira.SubmissionStream, error) {
	sendC := make(chan models.Submission, 100)
	s := &mira.SubmissionStream{
		C:     sendC,
		Close: make(chan struct{}),
	}
	_, err := c.Subreddit(name).Posts("new", "all", 1)
	if err != nil {
		return nil, err
	}
	var last models.RedditID
	go func() {
		sent := ring.New(100)
		for {
			select {
			case <-s.Close:
				close(sendC)
				return
			default:
			}
			posts, err := c.Subreddit(name).PostsAfter(last, 100)
			if err != nil {
				close(sendC)
				return
			}
			if len(posts) > 95 {
				//fmt.Printf("%d new posts | ", len(posts))
			}
			for i := len(posts) - 1; i >= 0; i-- {
				if ringContains(sent, posts[i].GetID()) {
					continue
				}
				sendC <- posts[i]
				sent.Value = posts[i].GetID()
				sent = sent.Next()
			}
			if len(posts) == 0 {
				last = ""
			} else if len(posts) > 2 {
				last = posts[1].GetID()
			}
			time.Sleep(15 * time.Second)
		}
	}()
	return s, nil
}

func streamSubredditComments(c *mira.Reddit, name string) (*mira.SubmissionStream, error) {
	sendC := make(chan models.Submission, 100)
	s := &mira.SubmissionStream{
		C:     sendC,
		Close: make(chan struct{}),
	}
	_, err := c.Subreddit(name).Posts("new", "all", 1)
	if err != nil {
		return nil, err
	}
	var last models.RedditID
	go func() {
		sent := ring.New(100)
		T := time.NewTicker(time.Millisecond * 1100)
		for {
			select {
			case <-s.Close:
				close(sendC)
				return
			default:
			}
			comments, err := c.Subreddit(name).CommentsAfter("new", last, 100)
			if err != nil {
				close(sendC)
				return
			}
			if len(comments) > 95 {
				//fmt.Printf("%d new comments | ", len(comments))
			}
			for i := len(comments) - 1; i >= 0; i-- {
				if ringContains(sent, comments[i].GetID()) {
					continue
				}
				//fmt.Print("a")
				sendC <- comments[i]
				sent.Value = comments[i].GetID()
				sent = sent.Next()
			}
			if len(comments) == 0 {
				last = ""
			} else if len(comments) > 2 {
				last = comments[1].GetID()
			}
			<-T.C
		}
	}()
	return s, nil
}

func ringContains(r *ring.Ring, n models.RedditID) bool {
	ret := false
	r.Do(func(p interface{}) {
		if p == nil {
			return
		}
		i := p.(models.RedditID)
		if i == n {
			ret = true
		}
	})
	return ret
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

func captureFunc(f func() error) (r bool) {
	err := f()
	if r = err != nil; r {
		_, file, no, ok := runtime.Caller(1)
		if ok {
			err = fmt.Errorf("%v from %s#%d", err, file, no)
		}
		go raven.CaptureError(err, nil)
	}
	return
}

func commentToMessage(comment *models.Comment) *reddit1.Message {
	if comment == nil {
		return nil
	}
	return &reddit1.Message{
		ID:         comment.ID,
		Name:       string(comment.GetID()),
		CreatedUTC: uint64(comment.CreatedUTC),
		Author:     comment.Author,
		Body:       comment.Body,
		BodyHTML:   comment.BodyHTML,
		Context:    comment.Permalink,
		ParentID:   string(comment.ParentID),
		Subreddit:  comment.Subreddit,
		WasComment: true,
	}
}

type RedditPageJSON []struct {
	Data struct {
		Children []struct {
			Data struct {
				MediaMetadata map[string]struct {
					Status  string `json:"status"`
					E       string `json:"e"`
					DashURL string `json:"dashUrl"`
					X       int    `json:"x"`
					Y       int    `json:"y"`
					HlsURL  string `json:"hlsUrl"`
					ID      string `json:"id"`
					IsGif   bool   `json:"isGif"`
				} `json:"media_metadata"`
				URL string `json:"url"`
			} `json:"data"`
		} `json:"children"`
		After  interface{} `json:"after"`
		Before interface{} `json:"before"`
	} `json:"data"`
}

type RedditStreamJSON struct {
	Data struct {
		Stream struct {
			StreamID string `json:"stream_id"`
			HlsURL   string `json:"hls_url"`
			State    string `json:"state"`
		} `json:"stream"`
	} `json:"data"`
}
