package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	irc "github.com/fluffle/goirc/client"
	"github.com/google/go-github/github"
	rss "github.com/jteeuwen/go-pkg-rss"
	"github.com/jteeuwen/go-pkg-xmlx"
	"golang.org/x/oauth2"
)

var (
	JenkinsRSSUrl  = getEnvOrdefault("JENKINS_RSS_URL", "https://ci.openshift.redhat.com/jenkins/job/test_pull_requests_origin/rssAll")
	GithubRepoOrg  = getEnvOrdefault("GITHUB_REPO_ORG", "openshift")
	GithubRepoName = getEnvOrdefault("GITHUB_REPO_NAME", "origin")
	IRCChannel     = getEnvOrdefault("IRC_JOIN_CHANNEL", "#mfojtik-test")
	IRCServer      = getEnvOrdefault("IRC_SERVER", "irc.devel.redhat.com:6667")
	IRCNick        = getEnvOrdefault("IRC_NICK", "os-jenkins")
)

var (
	watchLogins = []string{}
	client      *github.Client
	prChan      chan *PR
	ircMessages chan string
)

type PR struct {
	ID     int
	JobURL string
	Title  string
	Author string
	Status string
}

func (p *PR) Process() *PR {
	pr, _, err := client.PullRequests.Get(GithubRepoOrg, GithubRepoName, p.ID)
	if err != nil {
		return p
	}
	if user := pr.User; user != nil {
		p.Author = *user.Login
	}
	if title := pr.Title; title != nil {
		p.Title = *title
	}
	return p
}

func getEnvOrdefault(name, defaultValue string) string {
	if val := os.Getenv(name); len(val) > 0 {
		return val
	}
	return defaultValue
}

func pollRSSFeed(uri string, timeout int, cr xmlx.CharsetFunc) {
	feed := rss.New(timeout, true, chanHandler, itemHandler)
	for {
		if err := feed.Fetch(uri, cr); err != nil {
			fmt.Fprintf(os.Stderr, "[e] %s: %s\n", uri, err)
			return
		}
		<-time.After(time.Duration(feed.SecondsTillUpdate() * 1e9))
	}
}

func chanHandler(feed *rss.Feed, newchannels []*rss.Channel) {}

func itemHandler(feed *rss.Feed, ch *rss.Channel, newitems []*rss.Item) {
	var wg sync.WaitGroup
	wg.Add(len(newitems))
	count := 0
	for _, item := range newitems {
		count++
		go func(i rss.Item) {
			defer wg.Done()
			prHandler(i)
		}(*item)
		// Prevent IRC channel flooding
		if count > 5 {
			break
		}
	}
	wg.Wait()
}

func titleToState(title string) string {
	parts := strings.SplitN(title, " ", 3)
	if len(parts) != 3 {
		return "UNKNOWN"
	}
	status := parts[2]
	switch {
	case strings.Contains(status, "broken since"):
		return "STILL FAILING"
	case strings.Contains(status, "back to normal"):
		return "NOW FIXED"
	case strings.Contains(status, "stable"):
		return "STILL OK"
	case strings.Contains(status, "aborted"):
		return ""
	default:
		return "UNKNOWN(" + status + ")"
	}
}

func prHandler(item rss.Item) {
	if len(item.Links) == 0 {
		return
	}
	if item.Content == nil {
		return
	}
	status := titleToState(item.Title)
	processPR := func(content, href, status string) {
		// Parse pull request number from the github link in content
		s := content[strings.Index(content, "/pull/")+6:]
		num, err := strconv.Atoi(s[:strings.Index(s, `"`)])
		if err != nil {
			return
		}
		pr := &PR{JobURL: href, ID: num, Status: status}
		prChan <- pr.Process()
	}
	go processPR(item.Content.Text, item.Links[0].Href, status)
}

func setupIRC() {
	c := irc.SimpleClient(IRCNick)
	connected := make(chan bool, 1)
	c.EnableStateTracking()
	c.HandleFunc("connected", func(conn *irc.Conn, line *irc.Line) {
		conn.Join(IRCChannel)
		connected <- true
	})
	if err := c.ConnectTo(IRCServer); err != nil {
		fmt.Printf("[e] %v", err)
		os.Exit(1)
	}
	<-connected
	for {
		select {
		case msg := <-ircMessages:
			c.Notice(IRCChannel, msg)
		}
	}
	defer c.Quit("bye!")
}

func main() {
	ghToken := os.Getenv("GITHUB_TOKEN")
	if len(ghToken) == 0 {
		fmt.Println("You must set GITHUB_TOKEN. See https://github.com/settings/tokens")
		os.Exit(1)
	}
	ts := oauth2.StaticTokenSource(
		&oauth2.Token{AccessToken: ghToken},
	)
	tc := oauth2.NewClient(oauth2.NoContext, ts)
	client = github.NewClient(tc)
	prChan = make(chan *PR)
	ircMessages = make(chan string)
	go setupIRC()
	go func() {
		for {
			select {
			case pr := <-prChan:
				msg := fmt.Sprintf("PR#%d(\x02%s\x0F) authored by \x02%s\x0F is \x02%s\x0F", pr.ID, pr.Title, pr.Author, pr.Status)
				fmt.Println(msg)
				ircMessages <- msg
			}
		}
	}()
	pollRSSFeed(JenkinsRSSUrl, 5, nil)
}
