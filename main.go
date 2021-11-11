package main

import (
	"container/ring"
	"encoding/json"
	"fmt"
	"github.com/AudDMusic/audd-go"
	"github.com/Mihonarium/go-profanity"
	"github.com/fsnotify/fsnotify"
	"github.com/getsentry/raven-go"
	"github.com/kodova/html-to-markdown/escape"
	"github.com/ttgmpsn/mira"
	"github.com/ttgmpsn/mira/models"
	"github.com/turnage/graw"
	reddit1 "github.com/turnage/graw/reddit"
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
	SubredditsBannedOn    []string               `usage:"subreddits auddbot is banned on by BotDefense" json:"SubredditsBannedOn"`
	ApprovedOn            []string               `usage:"subreddits the bot can post links on" json:"ApprovedOn"`
	DontPostPatreonLinkOn []string               `usage:"subreddits not to post Patreon links on'" json:"DontPostPatreonLinkOn"`
	DontUseFormattingOn   []string               `usage:"subreddits not to use formatting on'" json:"DontUseFormattingOn"`
	ReplySettings         map[string]ReplyConfig `required:"true" json:"ReplySettings"`
	MaxTriggerTextLength  int                    `json:"MaxTriggerTextLength"`
	LiveStreamMinScore    int                    `json:"LiveStreamMinScore"`
	CommentsMinScore      int                    `json:"CommentsMinScore"`
	PatreonSupporters     []string               `json:"patreon_supporters"`
	RavenDSN              string                 `default:"" usage:"add a Sentry DSN to capture errors" json:"RavenDSN"`
	UserAgent             string                 `json:"UserAgent"`
	ClientID              string                 `json:"ClientID"`
	ClientSecret          string                 `json:"ClientSecret"`
	BotPasswords          map[string]string      `json:"BotPasswords"`
}

var stats = map[string]int{}
var statsMu = &sync.Mutex{}

func addToStats(t string) int {
	statsMu.Lock()
	defer statsMu.Unlock()
	stats[t]++
	return stats[t]
}

func (r *auddBot) getPatreonSupporter(botMentioned, fullMatch bool) string {
	if !botMentioned || !fullMatch {
		return ""
	}
	if len(r.config.PatreonSupporters) == 0 {
		return ""
	}
	i := addToStats("patreon shout-outs")
	return r.config.PatreonSupporters[i%len(r.config.PatreonSupporters)]
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
const AudDBotUsername = "auddbot"
const RecognizeSongBotUsername = "RecognizeSong"

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

func SecondsToTimeString(i int, includeHours bool) string {
	if includeHours {
		return fmt.Sprintf("%02d:%02d:%02d", i/3600, (i%3600)/60, i%60)
	}
	return fmt.Sprintf("%02d:%02d", i/60, i%60)
}

func GetTimeFromText(s string) (int, int) {
	s = strings.ReplaceAll(s, " - ", "")
	words := strings.Split(s, " ")
	Time := 0
	TimeTo := 0
	maxScore := 0
	for _, w := range words {
		score := 0
		w2 := ""
		if strings.Contains(w, "-") {
			w2 = strings.Split(w, "-")[1]
			w = strings.Split(w, "-")[0]
			score += 1
		}
		w = strings.TrimSuffix(w, "s")
		w2 = strings.TrimSuffix(w2, "s")
		if strings.Contains(w, ":") {
			score += 2
		}
		if score > maxScore {
			t, err := TimeStringToSeconds(w)
			if err == nil {
				Time = t
				TimeTo, _ = TimeStringToSeconds(w2) // if w2 is empty or not a correct time, TimeTo is 0
				maxScore = score
			}
		}
	}
	return Time, TimeTo
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

func isEmpty(e ...string) bool {
	for _, s := range e {
		if s != "" {
			return true
		}
	}
	return false
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
			song.Title = profanity.MaskProfanityWithoutKeepingSpaceTypes(song.Title, "*", 2)
			song.Artist = profanity.MaskProfanityWithoutKeepingSpaceTypes(song.Artist, "*", 2)
			song.Album = profanity.MaskProfanityWithoutKeepingSpaceTypes(song.Album, "*", 2)
			song.Label = profanity.MaskProfanityWithoutKeepingSpaceTypes(song.Label, "*", 2)
			song.Title = escape.Markdown(song.Title)
			song.Artist = escape.Markdown(song.Artist)
			song.Album = escape.Markdown(song.Album)
			song.Label = escape.Markdown(song.Label)
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
				releaseDate := ""
				if song.Title != song.Album && song.Album != "" {
					album = "Album: `" + song.Album + "`. "
				}
				if song.Artist != song.Label && song.Label != "Self-released" && song.Label != "" {
					label = " by `" + song.Label + "`"
				}
				if song.ReleaseDate != "" {
					releaseDate = "Released on `" + song.ReleaseDate + "`"
				} else {
					if label != "" {
						label = "Label: " + song.Label
					}
				}
				if !isEmpty(album, label, releaseDate) {
					text += fmt.Sprintf("\n\n%s%s%s.",
						album, song.ReleaseDate, label)
				}
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
	return response
}

type d struct {
	b bool
	u string
}

var avoidDuplicates = map[string]chan d{}
var avoidDoubleDuplicates = map[string]bool{}
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
			skip += tInt
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

const enterpriseChunkLength = 12

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
		avoidDoubleDuplicates[parentID] = true
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
		if avoidDoubleDuplicates[parentID] {
			avoidDuplicatesMu.Unlock()
			fmt.Println("Ignored a double duplicate")
			return
		}
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
			reply := "I'll listen to the next " + strconv.Itoa(enterpriseChunkLength) + " seconds of the stream and try to identify the song"
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
	timestampTo := 0
	timestamp := GetSkipFirstFromLink(resultUrl)
	if timestamp == 0 {
		timestamp, timestampTo = GetTimeFromText(body)
	}
	if timestampTo != 0 && timestampTo-timestamp > limit*enterpriseChunkLength {
		// recognize music at the middle of the specified interval
		timestamp += (timestampTo - timestamp - limit*enterpriseChunkLength) / 2
	}
	timestampTo = timestamp + limit*enterpriseChunkLength
	atTheEnd := "false"
	if timestamp == 0 && strings.Contains(body, "at the end") && !isLivestream {
		atTheEnd = "true"
	}
	result, err := r.audd.RecognizeLongAudio(resultUrl,
		map[string]string{"accurate_offsets": "true", "limit": strconv.Itoa(limit),
			"skip_first_seconds": strconv.Itoa(timestamp), "reversed_order": atTheEnd})
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
		"[Donate](https://www.reddit.com/r/AudD/comments/nua48w/please_consider_donating_and_making_the_bot_happy/)",
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
		highestScore := 0
		for i := range result {
			if result[i].Songs[0].Score > highestScore {
				highestScore = result[i].Songs[0].Score
			}
		}
		shoutOutToPatreonSupporter := r.getPatreonSupporter(summoned, highestScore == 100)
		if shoutOutToPatreonSupporter != "" {
			footerLinks[2] += " ^(Music recognition costs a lot. This result was brought to you by our Patreon supporter, " +
				shoutOutToPatreonSupporter + ")"
		} else {
			if highestScore == 100 {
				footerLinks[2] += " ^(Please consider supporting me on Patreon. Music recognition costs a lot)"
			}
		}
		if highestScore < 100 && !isLivestream {
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
		at := SecondsToTimeString(timestamp, timestampTo >= 3600) + "-" + SecondsToTimeString(timestampTo, timestampTo >= 3600)
		if atTheEnd == "true" {
			at = "the end"
		}
		response = fmt.Sprintf("Sorry, I couldn't recognize the song."+
			"\n\nI tried to identify music from the [link](%s) at %s.",
			resultUrl, at)
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
	if err != nil {
		capture(fmt.Errorf("%v from r/%s", err, subreddit))
	} else {
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

func getBannedText(subreddit string) string {
	return "Hi there,\n\nSorry, the bot was banned on r/" +  subreddit + " - probably automatically by BotDefense - so " +
		"it can't reply there. You can contact the subreddit's moderators and ask them to unban the bot. " +
		"You can also mention u/RecognizeSong instead."
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
	if stringInSlice([]string{"auddbot", "RecognizeSong"}, p.Author) {
		fmt.Println("Ignoring a comment from itself", p.Body)
		return nil
	}
	if stringInSlice(r.config.IgnoreSubreddits, p.Subreddit) {
		return nil
	}
	if stringInSlice(r.config.SubredditsBannedOn, p.Subreddit) && !strings.Contains(compare, "u/recognizesong") {
		avoidDuplicatesMu.Lock()
		if _, exists := avoidDuplicates[p.ParentID]; exists {
			avoidDuplicatesMu.Unlock()
			return nil
		}
		c := make(chan d, 1)
		avoidDuplicates[p.ParentID] = c
		avoidDuplicatesMu.Unlock()
		r.r2.Redditor(p.Author).Compose("Sorry, I'm banned on r/"+p.Subreddit, getBannedText(p.Subreddit))
		fmt.Println("Ignoring a mention from", p.Subreddit, "https://reddit.com"+p.Name)
		return nil
	}
	go func() {
		capture(r.r.Me().ReadMessage(p.Name))
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
		strings.Contains(compare, "stupid bot") || strings.ToLower(p.Body) == "wrong" {
		myRepliesMu.Lock()
		comment, exists := myReplies[p.ParentID]
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
				delete(myReplies, p.ParentID)
				myRepliesMu.Unlock()
			}
			fmt.Println("got a bad bot comment", "https://reddit.com//comments/"+p.ParentID+"/"+p.ID)
			// capture(r.r.ReadMessage(p.Name))
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
		return
	}
	if stringInSlice([]string{"auddbot", "RecognizeSong"}, p.Author) {
		fmt.Println("Ignoring a comment from itself", p.Body)
		return
	}
	if stringInSlice(r.config.IgnoreSubreddits, p.Subreddit) {
		fmt.Println("Ignoring a comment from", p.Subreddit, "https://reddit.com"+p.Permalink)
		return
	}
	if stringInSlice(r.config.SubredditsBannedOn, p.Subreddit) &&
		!strings.Contains(compare, "u/recognizesong") && strings.Contains(compare, "u/auddbot") {
		avoidDuplicatesMu.Lock()
		if _, exists := avoidDuplicates[string(p.ParentID)]; exists {
			avoidDuplicatesMu.Unlock()
			return
		}
		c := make(chan d, 1)
		avoidDuplicates[string(p.ParentID)] = c
		avoidDuplicatesMu.Unlock()
		r.r2.Redditor(p.Author).Compose("Sorry, I'm banned on r/"+p.Subreddit, getBannedText(p.Subreddit))
		fmt.Println("Ignoring a mention from", p.Subreddit, "https://reddit.com"+p.Name)
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
	if stringInSlice(r.config.IgnoreSubreddits, p.Subreddit) {
		fmt.Println("Ignoring a post from", p.Subreddit, "https://reddit.com"+p.Permalink)
		return
	}
	if stringInSlice(r.config.SubredditsBannedOn, p.Subreddit) &&
		!strings.Contains(compare, "u/recognizesong") && strings.Contains(compare, "u/auddbot") {
		avoidDuplicatesMu.Lock()
		if _, exists := avoidDuplicates[p.ID]; exists {
			avoidDuplicatesMu.Unlock()
			return
		}
		c := make(chan d, 1)
		avoidDuplicates[p.ID] = c
		avoidDuplicatesMu.Unlock()
		r.r2.Redditor(p.Author).Compose("Sorry, I'm banned on r/"+p.Subreddit, getBannedText(p.Subreddit))
		fmt.Println("Ignoring a mention from", p.Subreddit, "https://reddit.com"+p.Name)
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

func (c *BotConfig) getReddit2Credentials(username string) *mira.Reddit {
	r := mira.Init(mira.Credentials{
		ClientID:     c.ClientID,
		ClientSecret: c.ClientSecret,
		Username:     username,
		Password:     c.BotPasswords[username],
		UserAgent:    c.UserAgent,
	})
	err := r.LoginAuth()
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
func (c *BotConfig) getReddit1Credentials(username string) reddit1.Bot {
	bot, err := reddit1.NewBot(
		reddit1.BotConfig{
			Agent: c.UserAgent,
			App: reddit1.App{
				ID:       c.ClientID,
				Secret:   c.ClientSecret,
				Username: username,
				Password: c.BotPasswords[username],
			},
			Rate: 0,
		},
	)

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
			bot:    cfg.getReddit1Credentials(AudDBotUsername),
			bot2:   cfg.getReddit1Credentials(RecognizeSongBotUsername),
			audd:   audd.NewClient(cfg.AudDToken),
			r:      cfg.getReddit2Credentials(RecognizeSongBotUsername),
			r2:     cfg.getReddit2Credentials(AudDBotUsername),
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
