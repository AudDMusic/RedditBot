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
	"net/http"
	"net/url"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

func (r *auddBot) GetVideoLink(p *reddit2.Message) (string, error) {
	resultUrl := ""
	var lastPost *reddit.Post
	for parentId := p.ParentID; parentId != ""; {
		posts, comments, _, _, err := r.client.Listings.Get(context.Background(), parentId)
		j, _ := json.Marshal([]interface{}{posts, comments})
		fmt.Println(string(j))
		parentId = ""
		if err != nil {
			return "", err
		}
		if len(posts) > 0 {
			if posts[0] != nil {
				lastPost = posts[0]
				resultUrl = posts[0].URL
				break
			}
		}
		if len(comments) > 0 {
			if comments[0] != nil {
				parentId = comments[0].ParentID
			}
		}
	}
	if resultUrl == "" {
		j, _ := json.Marshal(lastPost)
		return "", fmt.Errorf("got a post without any URL (%s)", string(j))
	}
	if strings.Contains(resultUrl, "https://v.redd.it/") {
		resultUrl += "/DASH_audio.mp4"
	}
	if strings.Contains(resultUrl, "reddit.com/") {
		if lastPost != nil {
			if strings.Contains(lastPost.Body, "https://reddit.com/link/"+lastPost.ID+"/video/") {
				s := strings.Split(lastPost.Body, "https://reddit.com/link/"+lastPost.ID+"/video/")
				s = strings.Split(s[1], "/")
				resultUrl = "https://v.redd.it/" + s[0] + "/"
			}
		}
		if strings.Contains(resultUrl, "reddit.com/") {
			markdownRegex := regexp.MustCompile(`\[[^][]+]\((https?://[^()]+)\)`)
			results := markdownRegex.FindAllStringSubmatch(p.Body, -1)
			if lastPost != nil {
				results = append(results, markdownRegex.FindAllStringSubmatch(lastPost.Body, -1)...)
			}
			if len(results) != 0 {
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
		}
		if strings.Contains(resultUrl, "reddit.com/") {
			jsonUrl := resultUrl + ".json"
			resp, err := http.Get(jsonUrl)
			defer resp.Body.Close()
			if !capture(err) {
				var page RedditPageJSON
				body, err := ioutil.ReadAll(resp.Body)
				capture(err)
				fmt.Println(string(body))
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
	}
	if strings.Contains(resultUrl, "vocaroo.com/") || strings.Contains(resultUrl, "https://voca.ro/") {
		resultUrl = "https://media1.vocaroo.com/mp3/" + strings.Split(resultUrl, "/")[1]
	}
	return resultUrl, nil
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
				if tsInt, err := strconv.Atoi(s[1]); !capture(err) {
					tInt += tsInt
					if strings.Contains(s[0], "h") {
						s := strings.Split(s[0], "h")
						if tmInt, err := strconv.Atoi(s[1]); !capture(err) {
							tInt += tmInt * 60
						}
						if thInt, err := strconv.Atoi(s[0]); !capture(err) {
							tInt += thInt * 60 * 60
						}
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
			if song.Title != song.Album {
				album = "Album: ^(" + song.Album + "). "
			}
			if song.Artist != song.Label && song.Label != "Self-released" {
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
	if withLinks {
		response += "\n\n[**GitHub**](https://github.com/AudDMusic/RedditBot) | " +
			"[**Donate**](https://www.patreon.com/audd) | " +
			"[Feedback](https://www.reddit.com/message/compose?to=Mihonarium&subject=Music20recognition)"
	}
	return response
}

func (r *auddBot) Mention(p *reddit2.Message) error {
	j, _ := json.Marshal(p)
	fmt.Println(string(j))
	if !p.New {
		return nil
	}
	_, err := r.client.Message.Read(context.Background(), p.Name)
	capture(err)
	resultUrl, err := r.GetVideoLink(p)
	if capture(err) {
		return nil
	}
	skip := GetSkipFromLink(resultUrl)
	if skip == -1 {
		// ToDo: parse the comment that mentions the bot and check if there's a different timestamp
		// (only in the case the t parameter of the url doesn't have a timestamp? or not?)
	}
	fmt.Println(resultUrl)
	result, err := r.audd.RecognizeLongAudio(resultUrl,
		map[string]string{"accurate_offsets":"true", "skip":strconv.Itoa(skip), "limit":"3"}) //ToDo: maybe limit 2?
	if capture(err) {
		return nil
	}
	if len(result) == 0 && strings.Contains(resultUrl, "https://www.reddit.com/") {
		response := "Sorry, I couldn't get the video URL from the post or your comment.\n\n" +
			"[GitHub](https://github.com/AudDMusic/RedditBot/issues/new) " +
			"[^(new issue)](https://github.com/AudDMusic/RedditBot/issues/new) | " +
			"[Feedback](https://www.reddit.com/message/compose?to=Mihonarium&subject=Music20recognition)"
		fmt.Println(response)
		err = r.bot.Reply(p.Name, response)
		return nil
	}
	withLinks := !strings.Contains(p.Body, "without links")
	response := GetReply(result, withLinks)
	if response == "" {
		response = "Sorry, I couldn't recognize the song.\n\n" +
			"[GitHub](https://github.com/AudDMusic/RedditBot/issues/new) " +
			"[^(new issue)](https://github.com/AudDMusic/RedditBot/issues/new) | " +
			"[Feedback](https://www.reddit.com/message/compose?to=Mihonarium&subject=Music20recognition)"
	}
	fmt.Println(response)
	err = r.bot.Reply(p.Name, response)
	capture(err)
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

	client.Stream.Posts("")

	bot, err := reddit2.NewBotFromAgentFile("auddbot.agent", 0)
	if capture(err) {
		panic(err)
	}

	//cfg := graw.Config{Subreddits: []string{"bottesting"}, Mentions: true}
	cfg := graw.Config{Mentions: true}
	handler := &auddBot{bot: bot, client: client, audd: audd.NewClient("test")}
	// See https://docs.audd.io/enterprise
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


type RedditPageJSON []struct {
	Kind string `json:"kind"`
	Data struct {
		Modhash  string `json:"modhash"`
		Dist     int    `json:"dist"`
		Children []struct {
			Kind string `json:"kind"`
			Data struct {
				ApprovedAtUtc         interface{}   `json:"approved_at_utc"`
				Subreddit             string        `json:"subreddit"`
				Selftext              string        `json:"selftext"`
				UserReports           []interface{} `json:"user_reports"`
				Saved                 bool          `json:"saved"`
				ModReasonTitle        interface{}   `json:"mod_reason_title"`
				Gilded                int           `json:"gilded"`
				Clicked               bool          `json:"clicked"`
				Title                 string        `json:"title"`
				LinkFlairRichtext     []interface{} `json:"link_flair_richtext"`
				SubredditNamePrefixed string        `json:"subreddit_name_prefixed"`
				Hidden                bool          `json:"hidden"`
				Pwls                  int           `json:"pwls"`
				LinkFlairCSSClass     string        `json:"link_flair_css_class"`
				Downs                 int           `json:"downs"`
				ThumbnailHeight       interface{}   `json:"thumbnail_height"`
				TopAwardedType        interface{}   `json:"top_awarded_type"`
				ParentWhitelistStatus string        `json:"parent_whitelist_status"`
				HideScore             bool          `json:"hide_score"`
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
				Name                       string      `json:"name"`
				Quarantine                 bool        `json:"quarantine"`
				LinkFlairTextColor         string      `json:"link_flair_text_color"`
				UpvoteRatio                float64     `json:"upvote_ratio"`
				AuthorFlairBackgroundColor interface{} `json:"author_flair_background_color"`
				SubredditType              string      `json:"subreddit_type"`
				Ups                        int         `json:"ups"`
				TotalAwardsReceived        int         `json:"total_awards_received"`
				MediaEmbed                 struct {
				} `json:"media_embed"`
				ThumbnailWidth        interface{} `json:"thumbnail_width"`
				AuthorFlairTemplateID interface{} `json:"author_flair_template_id"`
				IsOriginalContent     bool        `json:"is_original_content"`
				AuthorFullname        string      `json:"author_fullname"`
				SecureMedia           interface{} `json:"secure_media"`
				IsRedditMediaDomain   bool        `json:"is_reddit_media_domain"`
				IsMeta                bool        `json:"is_meta"`
				Category              interface{} `json:"category"`
				SecureMediaEmbed      struct {
				} `json:"secure_media_embed"`
				LinkFlairText       string        `json:"link_flair_text"`
				CanModPost          bool          `json:"can_mod_post"`
				Score               int           `json:"score"`
				ApprovedBy          interface{}   `json:"approved_by"`
				AuthorPremium       bool          `json:"author_premium"`
				Thumbnail           string        `json:"thumbnail"`
				Edited              bool       `json:"edited"`
				AuthorFlairCSSClass interface{}   `json:"author_flair_css_class"`
				AuthorFlairRichtext []interface{} `json:"author_flair_richtext"`
				Gildings            struct {
				} `json:"gildings"`
				ContentCategories        interface{}   `json:"content_categories"`
				IsSelf                   bool          `json:"is_self"`
				ModNote                  interface{}   `json:"mod_note"`
				Created                  float64       `json:"created"`
				LinkFlairType            string        `json:"link_flair_type"`
				Wls                      int           `json:"wls"`
				RemovedByCategory        interface{}   `json:"removed_by_category"`
				BannedBy                 interface{}   `json:"banned_by"`
				AuthorFlairType          string        `json:"author_flair_type"`
				Domain                   string        `json:"domain"`
				AllowLiveComments        bool          `json:"allow_live_comments"`
				SelftextHTML             string        `json:"selftext_html"`
				Likes                    interface{}   `json:"likes"`
				SuggestedSort            string        `json:"suggested_sort"`
				BannedAtUtc              interface{}   `json:"banned_at_utc"`
				ViewCount                interface{}   `json:"view_count"`
				Archived                 bool          `json:"archived"`
				NoFollow                 bool          `json:"no_follow"`
				IsCrosspostable          bool          `json:"is_crosspostable"`
				Pinned                   bool          `json:"pinned"`
				Over18                   bool          `json:"over_18"`
				AllAwardings             []interface{} `json:"all_awardings"`
				Awarders                 []interface{} `json:"awarders"`
				MediaOnly                bool          `json:"media_only"`
				LinkFlairTemplateID      string        `json:"link_flair_template_id"`
				CanGild                  bool          `json:"can_gild"`
				Spoiler                  bool          `json:"spoiler"`
				Locked                   bool          `json:"locked"`
				AuthorFlairText          interface{}   `json:"author_flair_text"`
				TreatmentTags            []interface{} `json:"treatment_tags"`
				Visited                  bool          `json:"visited"`
				RemovedBy                interface{}   `json:"removed_by"`
				NumReports               interface{}   `json:"num_reports"`
				Distinguished            interface{}   `json:"distinguished"`
				SubredditID              string        `json:"subreddit_id"`
				ModReasonBy              interface{}   `json:"mod_reason_by"`
				RemovalReason            interface{}   `json:"removal_reason"`
				LinkFlairBackgroundColor string        `json:"link_flair_background_color"`
				ID                       string        `json:"id"`
				IsRobotIndexable         bool          `json:"is_robot_indexable"`
				NumDuplicates            int           `json:"num_duplicates"`
				ReportReasons            interface{}   `json:"report_reasons"`
				Author                   string        `json:"author"`
				DiscussionType           interface{}   `json:"discussion_type"`
				NumComments              int           `json:"num_comments"`
				SendReplies              bool          `json:"send_replies"`
				Media                    interface{}   `json:"media"`
				ContestMode              bool          `json:"contest_mode"`
				AuthorPatreonFlair       bool          `json:"author_patreon_flair"`
				AuthorFlairTextColor     interface{}   `json:"author_flair_text_color"`
				Permalink                string        `json:"permalink"`
				WhitelistStatus          string        `json:"whitelist_status"`
				Stickied                 bool          `json:"stickied"`
				URL                      string        `json:"url"`
				SubredditSubscribers     int           `json:"subreddit_subscribers"`
				CreatedUtc               float64       `json:"created_utc"`
				NumCrossposts            int           `json:"num_crossposts"`
				ModReports               []interface{} `json:"mod_reports"`
				IsVideo                  bool          `json:"is_video"`
			} `json:"data"`
		} `json:"children"`
		After  interface{} `json:"after"`
		Before interface{} `json:"before"`
	} `json:"data"`
}
