package main

import (
	"bytes"
	"container/ring"
	"encoding/json"
	"fmt"
	"github.com/AudDMusic/audd-go"
	"github.com/fsnotify/fsnotify"
	"github.com/getsentry/raven-go"
	"github.com/golang/protobuf/proto"
	"github.com/surajJha/go-profanity-hindi"
	"github.com/ttgmpsn/mira"
	"github.com/ttgmpsn/mira/models"
	"github.com/turnage/graw"
	reddit1 "github.com/turnage/graw/reddit"
	"github.com/turnage/redditproto"
	"golang.org/x/time/rate"
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

type BotConfig struct {
	AudDToken             string                 `required:"true" default:"test" usage:"the token from dashboard.audd.io" json:"AudDToken"`
	Triggers              []string               `usage:"phrases bot will react to" json:"Triggers"`
	AntiTriggers          []string               `usage:"phrases bot will avoid replying to" json:"AudDTAntiTriggers"`
	IgnoreSubreddits      []string               `usage:"subreddits to ignore" json:"IgnoreSubreddits"`
	ApprovedOn            []string               `usage:"subreddits the bot can post links on" json:"ApprovedOn"`
	DontPostPatreonLinkOn []string               `usage:"subreddits not to post Patreon links on'" json:"DontPostPatreonLinkOn"`
	DontUseFormattingOn   []string               `usage:"subreddits not to use formatting on'" json:"DontUseFormattingOn"`
	ReplySettings         map[string]ReplyConfig `required:"true" json:"ReplySettings"`
	MaxTriggerTextLength  int                    `json:"MaxTriggerTextLength"`
	LiveStreamMinScore    int                    `json:"LiveStreamMinScore"`
	CommentsMinScore      int                    `json:"CommentsMinScore"`
	RavenDSN              string                 `default:"" usage:"add a Sentry DSN to capture errors" json:"RavenDSN"`
}

type ReplyConfig struct {
	SendLinks   bool `required:"true" usage:"should bot share links in the first reply" json:"SendLinks"`
	ReplyAlways bool `required:"true" usage:"will bot reply when hasn't recognized a song" json:"ReplyAlways"`
}

var commentsCounter int64 = 0
var postsCounter int64 = 0

var markdownRegex = regexp.MustCompile(`\[[^][]+]\((https?://[^()]+)\)`)
var rxStrict = xurls.Strict()

const configFile = "config.json"
const AudDBotConfig = "auddbot.agent"
const RecognizeSongBotConfig = "RecognizeSong.agent"

func stringInSlice(slice []string, s string) bool {
	for i := range slice {
		if s == slice[i] {
			return true
		}
	}
	return false
}
func substringInSlice(s string, slice []string) bool {
	for i := range slice {
		if strings.Contains(s, slice[i]) {
			return true
		}
	}
	return false
}

func TimeStringToSeconds(s string) (int, error) {
	list := strings.Split(s, ":")
	if len(list) > 3 {
		return 0, fmt.Errorf("too many : thingies")
	}
	result, multiplier := 0, 1
	for i := len(list) - 1; i >= 0; i-- {
		c, err := strconv.Atoi(list[i])
		if err != nil {
			return 0, err
		}
		result += c * multiplier
		multiplier *= 60
	}
	return result, nil
}

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
		if resultUrl != "" {
			return resultUrl, nil
		}
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
					if page[0].Data.Children[0].Data.RpanVideo.HlsURL != "" {
						resultUrl = page[0].Data.Children[0].Data.RpanVideo.HlsURL
					}
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

func GetReply(result []audd.RecognitionEnterpriseResult, withLinks, matched, full bool, minScore int) string {
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
			if song.Score < minScore {
				continue
			}
			if song.SongLink == "https://lis.tn/rvXTou" || song.SongLink == "https://lis.tn/XIhppO" {
				song.Artist = "The Caretaker (Leyland James Kirby)"
				song.Title = "Everywhere at the End of Time - Stage 1"
				song.Album = "Everywhere at the End of Time - Stage 1"
				song.ReleaseDate = "2016-09-22"
				song.SongLink = "https://www.youtube.com/watch?v=wJWksPWDKOc"
			}
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
			if matched {
				text += fmt.Sprintf(" (%s; matched: `%s`)", song.Timecode, score)
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
				text += fmt.Sprintf("\n\n%sReleased on `%s`%s.",
					album, song.ReleaseDate, label)
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
		for _, text := range texts {
			// response += fmt.Sprintf("\n\n%d. %s", i+1, text)
			response += fmt.Sprintf("\n\n• %s", text)
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

var myReplies = map[string]myComment{}

type myComment struct {
	summoned            bool
	commentID           string
	additionalCommentID string
}

var myRepliesMu = &sync.Mutex{}

func GetSkipFirstFromLink(Url string) int {
	skip := 0
	if strings.HasSuffix(Url, ".m3u8") {
		return skip
	}
	u, err := url.Parse(Url)
	if err == nil {
		t := u.Query().Get("t")
		if t == "" {
			t = u.Query().Get("time_continue")
			if t == "" {
				t = u.Query().Get("start")
			}
		}
		if t != "" {
			t = strings.ToLower(strings.ReplaceAll(t, "s", ""))
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
			skip += tInt / 12
			fmt.Println("skip:", skip)
		}
	}
	return skip
}

func removeFormatting(response string) string {
	response = strings.ReplaceAll(response, "**", "*")
	response = strings.ReplaceAll(response, "*", "\\*")
	response = strings.ReplaceAll(response, "`", "'")
	response = strings.ReplaceAll(response, "^", "")
	return response
}

func (r *auddBot) HandleQuery(mention *reddit1.Message, comment *models.Comment, post *models.Post) {
	var resultUrl, t, parentID, body, subreddit string
	var err error
	if mention != nil {
		t, parentID, body, subreddit = "mention", mention.Name, mention.Body, mention.Subreddit
		fmt.Println("\n ! Processing the mention")
	}
	if comment != nil {
		t, parentID, body, subreddit = "comment", string(comment.GetID()), comment.Body, comment.Subreddit
	}
	if post != nil {
		t, parentID, body, subreddit = "post", string(post.GetID()), post.Selftext, post.Subreddit
	}

	body = strings.ToLower(body)
	rs := strings.Contains(body, "recognizesong")
	summoned := rs || strings.Contains(body, "auddbot")

	if len(body) > r.config.MaxTriggerTextLength && r.config.MaxTriggerTextLength != 0 && !summoned {
		fmt.Println("The comment is too long, skipping", body)
		return
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

	if strings.Contains(resultUrl, "https://lis.tn/") {
		fmt.Println("Skipping a reply to our comment")
		return
	}
	if resultUrl == previousUrl {
		fmt.Println("Got the same URL, skipping")
		return
	}
	fmt.Println(resultUrl)
	limit := 2
	if strings.Contains(resultUrl, "v.redd.it") {
		limit = 3
	}
	isLivestream := strings.HasSuffix(resultUrl, ".m3u8")
	if isLivestream {
		fmt.Println("\nGot a livestream", resultUrl)
		if summoned {
			reply := "I'll listen to the next 12 seconds of the stream and try to identify the song"
			if rs {
				go r.r.ReplyWithID(parentID, reply)

			} else {
				go r.r2.ReplyWithID(parentID, reply)
			}
		}
		limit = 1
	}
	minScore := r.config.CommentsMinScore
	if isLivestream {
		minScore = r.config.LiveStreamMinScore
	}
	withLinks := (summoned || r.config.ReplySettings[t].SendLinks || stringInSlice(r.config.ApprovedOn, subreddit)) &&
		!strings.Contains(body, "without links") && !strings.Contains(body, "/wl") || isLivestream
	// Note that the enterprise endpoint will introduce breaking changes for how the skip parameter is used here
	result, err := r.audd.RecognizeLongAudio(resultUrl,
		map[string]string{"accurate_offsets": "true", "use_timecode": "true", "limit": strconv.Itoa(limit)})
	useFormatting := !stringInSlice(r.config.DontUseFormattingOn, subreddit)
	response := GetReply(result, withLinks, true, !isLivestream, minScore)
	if err != nil {
		if v, ok := err.(*audd.Error); ok {
			if v.ErrorCode == 501 {
				response = fmt.Sprintf("Sorry, I couldn't get any audio from the [link](%s)", resultUrl)
				if !r.config.ReplySettings[t].ReplyAlways && !summoned {
					fmt.Println("not summoned and couldn't get any audio, exiting")
					return
				}
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
		"[Donate](https://www.reddit.com/user/auddbot/comments/nuac09/please_consider_donating_and_making_the_bot_happy/)",
		//"[Feedback](/message/compose?to=Mihonarium&subject=Music%20recognition%20" + parentID + ")",
	}
	donateLink := 2
	if response == "" || len(result) == 0 {
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
		if result[0].Songs[0].Score == 100 {
			footerLinks[2] += " ^(If I helped you, please consider supporting me on Patreon. Music recognition costs a lot)"
		}
		if result[0].Songs[0].Score < 100 && !isLivestream {
			footerLinks[0] += " | If the matched percent is less than 100, it could be a false positive result. " +
				"I'm still posting it, because sometimes I get it right even if I'm not sure, so it could be helpful. " +
				"But please don't be mad at me if I'm wrong! I'm trying my best!"
		}
		c <- d{true, resultUrl}
	}
	if strings.Contains(body, "find-song") && !summoned {
		footerLinks[0] += " | ^(find-song went offline; its creator gave me a permission to react to the find-song mentions)"
	}
	//if len(result) == 0 || !summoned {
	if len(result) == 0 || stringInSlice(r.config.DontPostPatreonLinkOn, subreddit) {
		footerLinks = append(footerLinks[:donateLink], footerLinks[donateLink+1:]...)
	}
	footer := "\n\n" + strings.Join(footerLinks, " | ")

	if isLivestream {
		fmt.Println("\nStream results:", result)
	}
	if response == "" {
		timestamp := GetSkipFirstFromLink(resultUrl) * 12
		response = fmt.Sprintf("Sorry, I couldn't recognize the song."+
			"\n\nI tried to identify music from the [link](%s) at %d-%d seconds.",
			resultUrl, timestamp, timestamp+limit*12)
		if strings.Contains(resultUrl, "https://www.reddit.com/") {
			response = "Sorry, I couldn't get the video URL from the post or your comment."
		}
	}
	if withLinks {
		response += footer
	}
	if !useFormatting {
		response = removeFormatting(response)
	}
	// fmt.Println(response)
	var cr *models.CommentActionResponse
	if rs {
		cr, err = r.r.ReplyWithID(parentID, response)
	} else {
		cr, err = r.r2.ReplyWithID(parentID, response)
	}
	if !capture(err) {
		if len(cr.JSON.Data.Things) > 0 {
			sentID := string(cr.JSON.Data.Things[0].Data.GetID())
			comment := myComment{
				summoned:  summoned,
				commentID: sentID,
			}
			if !withLinks {
				response = "Links to the streaming platforms:\n\n"
				response += GetReply(result, true, false, false, minScore)
				response += footer
				if !useFormatting {
					response = removeFormatting(response)
				}
				if rs {
					cr, err = r.r.ReplyWithID(sentID, response)
				} else {
					cr, err = r.r2.ReplyWithID(sentID, response)
				}
				if !capture(err) {
					if len(cr.JSON.Data.Things) > 0 {
						sentID := string(cr.JSON.Data.Things[0].Data.GetID())
						comment.additionalCommentID = sentID
						myRepliesMu.Lock()
						myReplies[sentID] = comment
						myRepliesMu.Unlock()
					}
				}
			}
			myRepliesMu.Lock()
			myReplies[sentID] = comment
			myRepliesMu.Unlock()
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
	if substringInSlice(compare, r.config.AntiTriggers) {
		fmt.Println("Got an anti-trigger", p.Body)
		return nil
	}
	go func() {
		capture(r.r.ReadMessage(p.Name))
	}()
	r.HandleQuery(p, nil, nil)
	return nil
}
func (r *auddBot) CommentReply(p *reddit1.Message) error {
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
	if substringInSlice(compare, r.config.AntiTriggers) {
		fmt.Println("Got an anti-trigger", p.Body)
		return nil
	}

	if strings.Contains(compare, "bad bot") || strings.Contains(compare, "damn bot") ||
		strings.Contains(compare, "stupid bot") {
		myRepliesMu.Lock()
		comment, exists := myReplies[string(p.ParentID)]
		myRepliesMu.Unlock()
		if exists && !comment.summoned {
			var err2 error
			target := mira.RedditOauth + "/api/del"
			_, err := r.r2.MiraRequest("POST", target, map[string]string{
				"id":       comment.commentID,
				"api_type": "json",
			})

			if comment.additionalCommentID != "" {
				_, err2 = r.r2.MiraRequest("POST", target, map[string]string{
					"id":       comment.additionalCommentID,
					"api_type": "json",
				})
			}
			if !capture(err) && !capture(err2) {
				myRepliesMu.Lock()
				delete(myReplies, string(p.ParentID))
				myRepliesMu.Unlock()
			}
			fmt.Println("got a bad bot comment", "https://reddit.com//comments/"+p.ParentID+"/"+p.ID)
			capture(r.r.ReadMessage(p.Name))
		}
	}
	return nil
}
func replaceSlice(s, new string, oldStrings ...string) string {
	for _, old := range oldStrings {
		s = strings.ReplaceAll(s, old, new)
	}
	return s
}
func getBodyToCompare(body string) string {
	return "\n" + strings.ReplaceAll(strings.ToLower(replaceSlice(body, "", "'", "’", "`")), "what is", "whats") + "?"
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
	trigger := substringInSlice(compare, r.config.Triggers)
	if strings.Contains(compare, "bad bot") || strings.Contains(compare, "damn bot") ||
		strings.Contains(compare, "stupid bot") {
		myRepliesMu.Lock()
		comment, exists := myReplies[string(p.ParentID)]
		myRepliesMu.Unlock()
		if exists && !comment.summoned {
			target := mira.RedditOauth + "/api/del"
			_, err := r.r2.MiraRequest("POST", target, map[string]string{
				"id":       comment.commentID,
				"api_type": "json",
			})
			capture(err)
			if comment.additionalCommentID != "" {
				_, err = r.r2.MiraRequest("POST", target, map[string]string{
					"id":       comment.additionalCommentID,
					"api_type": "json",
				})
			}
			capture(err)
			fmt.Println("got a bad bot comment", "https://reddit.com"+p.Permalink)
			return
		}
	}
	if !trigger {
		d := minDistance(compare, "what", "song", "music", "track")
		if len(compare) < 100 && d != 0 && d < 10 {
			fmt.Println("Skipping comment:", compare, "https://reddit.com"+p.Permalink)
		}
		return
	}
	myRepliesMu.Lock()
	_, exists := myReplies[string(p.ParentID)]
	myRepliesMu.Unlock()
	if exists {
		fmt.Println("Ignoring a comment that's a reply to ours:", "https://reddit.com"+p.Permalink)
	}
	if stringInSlice(r.config.IgnoreSubreddits, p.Subreddit) {
		fmt.Println("Ignoring a comment from", p.Subreddit, "https://reddit.com"+p.Permalink)
		return
	}
	if substringInSlice(compare, r.config.AntiTriggers) {
		fmt.Println("Got an anti-trigger", p.Body, "https://reddit.com"+p.Permalink)
		return
	}
	//j, _ := json.Marshal(p)
	// fmt.Println("\n😻 Got a comment", "https://reddit.com"+p.Permalink, p.Body)
	r.HandleQuery(nil, p, nil)
	return
}

func (r *auddBot) Post(p *models.Post) {
	atomic.AddInt64(&postsCounter, 1)
	compare := getBodyToCompare(p.Selftext)
	trigger := substringInSlice(compare, r.config.Triggers) ||
		substringInSlice(strings.ToLower(p.Title), r.config.Triggers)
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
	bot    reddit1.Bot
	bot2   reddit1.Bot
	audd   *audd.Client
	r      *mira.Reddit
	r2     *mira.Reddit
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
	f, err := os.Open(file)
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
	if stringInSlice(cfg.Triggers, "") {
		return nil, fmt.Errorf("got a config with an empty string in the triggers")
	}
	if stringInSlice(cfg.AntiTriggers, "") {
		return nil, fmt.Errorf("got a config with an empty string in the anti-triggers")
	}
	return &cfg, nil
}

func WatchChanges(filename string, updated chan struct{}, l *rate.Limiter) {
	watcher, err := fsnotify.NewWatcher()
	capture(err)
	defer captureFunc(watcher.Close)
	done := make(chan bool)
	go func() {
		for {
			select {
			case event := <-watcher.Events:
				fmt.Println(event)
				if l != nil {
					if l.Allow() {
						updated <- struct{}{}
					} else {
						fmt.Println("skipping")
					}
				}
			case err := <-watcher.Errors:
				capture(err)
			}
		}
	}()
	if err := watcher.Add(filename); err != nil {
		capture(err)
	}
	<-done
}

func main() {
	// ToDo: get the config filename from a parameter
	// ToDo: move agents settings to the config
	cfg, err := loadConfig(configFile)
	if err != nil {
		panic(err)
	}
	if err := raven.SetDSN(cfg.RavenDSN); err != nil {
		panic(err)
	}
	reloadLimiter := rate.NewLimiter(1, 1)
	configUpdated := make(chan struct{}, 1)
	go WatchChanges(configFile, configUpdated, reloadLimiter)
	for {
		stop := make(chan struct{}, 2)
		cfg, err := loadConfig(configFile)
		if err != nil {
			capture(err)
			time.Sleep(15 * time.Second)
			continue
		}
		var grawStopChan = make(chan func(), 2)
		go func() {
			select {
			case <-configUpdated:
				fmt.Println("Waiting 2 seconds, reloading config and restarting")
				time.Sleep(time.Second * 2)
				stop <- struct{}{}
				select {
				case grawStop := <-grawStopChan:
					grawStop()
				default:
				}
				select {
				case grawStop := <-grawStopChan:
					grawStop()
				default:
				}
				return
			case <-stop:
				stop <- struct{}{}
				return
			}
		}()
		handler := &auddBot{
			config: *cfg,
			bot:    getReddit1Credentials(AudDBotConfig),
			bot2:   getReddit1Credentials(RecognizeSongBotConfig),
			audd:   audd.NewClient(cfg.AudDToken),
			r:      getReddit2Credentials(RecognizeSongBotConfig),
			r2:     getReddit2Credentials(AudDBotConfig),
		}
		if handler.bot == nil || handler.bot2 == nil || handler.r == nil || handler.r2 == nil {
			time.Sleep(time.Second * 10)
			continue
		}
		go func() {
			t := time.NewTicker(time.Minute)
			for {
				select {
				case <-stop:
					stop <- struct{}{}
					return
				case <-t.C:
				}
				newComments, newPosts := atomic.LoadInt64(&commentsCounter), atomic.LoadInt64(&postsCounter)
				fmt.Println("Comments:", newComments, "posts:", newPosts)
				atomic.AddInt64(&commentsCounter, -1*newComments)
				atomic.AddInt64(&postsCounter, -1*newPosts)
				if newComments == 0 {
					select {
					case grawStop := <-grawStopChan:
						grawStop()
					default:
					}
					select {
					case grawStop := <-grawStopChan:
						grawStop()
					default:
					}
					return
				}
			}
		}()
		handler.audd.SetEndpoint(audd.EnterpriseAPIEndpoint) // See https://docs.audd.io/enterprise
		/*_, err := handler.r.ListUnreadMessages()
		if capture(err) {
			capture(err)
			time.Sleep(15 * time.Second)
			continue
		}
		for i := range m {
			//go handler.Comment(m[i])
			fmt.Println(m[i].Permalink)
			capture(handler.r.ReadMessage(m[i].ID))
		}*/
		grawCfg := graw.Config{Mentions: true, CommentReplies: true}
		grawStop, wait, err := graw.Run(handler, handler.bot, grawCfg)
		if capture(err) {
			capture(err)
			time.Sleep(15 * time.Second)
			continue
		}
		grawStop2, wait2, err := graw.Run(handler, handler.bot2, grawCfg)
		if capture(err) {
			capture(err)
			time.Sleep(15 * time.Second)
			continue
		}
		grawStopChan <- grawStop
		grawStopChan <- grawStop2
		//ToDo: also get inbox messages to avoid not replying to mentions

		postsStream, err := streamSubredditPosts(handler.r, "all")
		if err != nil {
			capture(err)
			time.Sleep(15 * time.Second)
			continue
		}
		commentsStream, err := streamSubredditComments(handler.r2, "all")
		if err != nil {
			capture(err)
			time.Sleep(15 * time.Second)
			continue
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
		fmt.Println("graw run failed: ", wait(), wait2())
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
			time.Sleep(13 * time.Second)
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
		T := time.NewTicker(time.Millisecond * 1300)
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
				URL       string `json:"url"`
				RpanVideo struct {
					HlsURL           string `json:"hls_url"`
					ScrubberMediaURL string `json:"scrubber_media_url"`
				} `json:"rpan_video"`
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
