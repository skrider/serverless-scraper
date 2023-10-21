package scrape

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
  "path"

	"github.com/gocolly/colly"
	"github.com/passage-inc/chatassist/packages/vercel/common"
	gogpt "github.com/sashabaranov/go-gpt3"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo/options"
)

type pageChunk struct {
	route string
	text  string
	index int // order encountered by Colly
	level int // header level
}

// this function from net/url must be included explicitly as Vercel does not have the
// latest version of Go running in production, and url.JoinPath is only included in the
// latest version of the standard library
func JoinPathPolyfill(u *url.URL, elem ...string) *url.URL {
	elem = append([]string{u.EscapedPath()}, elem...)
	var p string
	if !strings.HasPrefix(elem[0], "/") {
		// Return a relative path if u is relative,
		// but ensure that it contains no ../ elements.
		elem[0] = "/" + elem[0]
		p = path.Join(elem...)[1:]
	} else {
		p = path.Join(elem...)
	}
	// path.Join will remove any trailing slashes.
	// Preserve at least one.
	if strings.HasSuffix(elem[len(elem)-1], "/") && !strings.HasSuffix(p, "/") {
		p += "/"
	}
	url := *u
	url.Path = p
	return &url
}

func resolveInlineUrl(loc *url.URL, inline string) *url.URL {
  parsedInline, _ := url.Parse(inline)
  if parsedInline.IsAbs() {
    return parsedInline
  }

  resolvedUrl, _ := url.Parse(loc.String())
  resolvedUrl.Fragment = parsedInline.Fragment

  locDir := loc.Path
  if len(path.Ext(locDir)) > 0 {
    locDir = path.Dir(locDir)
  }

  resolvedUrl.Path = locDir
  return JoinPathPolyfill(resolvedUrl, parsedInline.Path)
}

func parseParagraph(e *colly.HTMLElement) pageChunk {
	text := ""
	// find all anchor elements contained within e. Convert them to the markdown format
	a := e.DOM.Find("a")
	for i := 0; i < a.Length(); i++ {
		href := a.Eq(i).AttrOr("href", "")
		// if href is relative, convert it to a URL
    resolvedUrl := resolveInlineUrl(e.Request.URL, href)
		a.Eq(i).ReplaceWithHtml(fmt.Sprintf("[%s](%s)", a.Eq(i).Text(), resolvedUrl.String()))
	}
	// stringify the DOM
	text, _ = url.QueryUnescape(e.DOM.Text())

	return pageChunk{
		text:  strings.TrimSpace(text),
		level: 7,
	}
}

func parsePre(e *colly.HTMLElement) pageChunk {
	return pageChunk{
		text:  e.DOM.Text(),
		level: 7,
	}
}

func parseHeader(e *colly.HTMLElement) pageChunk {
	level, _ := strconv.Atoi(strings.TrimLeft(e.Name, "h"))

	return pageChunk{
		text:  strings.TrimSpace(e.DOM.Text()),
		level: level,
	}
}

const MAX_SECTION_PSEUDO_TOKENS = 600

func processPage(a []pageChunk) common.Page {
	// create temp array for mapping operations
	var temp []pageChunk

	// sort chunks by index
	sort.Slice(a, func(i, j int) bool {
		return a[i].index < a[j].index
	})

	// filter empty chunks
	temp = make([]pageChunk, 0, len(a))
	for _, v := range a {
		if len(v.text) > 0 {
			temp = append(temp, v)
		}
	}
	a = temp

	// append leaders
	temp = make([]pageChunk, len(a))
	for i, v := range a {
		level := v.level
		if v.level < 7 {
			v.text = strings.Repeat("#", level) + " " + v.text
		}
		temp[i] = v
	}
	a = temp

	// combine chunks into subpages
	sections := make([]common.Section, 1)
	// first chunk only here to make algo cleaner, will get dropped
	sections[0] = common.Section{
		Title:     "",
		Content:   "",
		Embedding: []float64{},
	}
	ptr := 0
	runningTokenCt := 0
	for _, v := range a {
		if v.level < 3 {
			ptr += 1
			sections = append(sections, common.Section{
				Title:     v.text,
				Content:   "",
				Embedding: []float64{},
			})
			runningTokenCt = common.CountPseudoTokens(v.text)
		} else if runningTokenCt > MAX_SECTION_PSEUDO_TOKENS {
			ptr += 1
			sections = append(sections, common.Section{
				Title:     sections[ptr-1].Title + " CONTINUED",
				Content:   v.text,
				Embedding: []float64{},
			})
			runningTokenCt = common.CountPseudoTokens(v.text)
		} else {
			sections[ptr].Content += "\n\n" + v.text
			runningTokenCt += common.CountPseudoTokens(v.text)
		}
	}
	sections = sections[1:]

	// filter out empty subpages
	nonEmptySections := make([]common.Section, 0, len(sections))
	for _, v := range sections {
		if len(v.Content) > 0 {
			nonEmptySections = append(nonEmptySections, v)
		}
	}

	// get title safely
	title := "untitled"
	if len(sections) > 0 {
		title = sections[0].Title
	}

	// return the completed page
	return common.Page{
		Title:    title,
		Sections: nonEmptySections,
	}
}

const SCRAPER_PARALLELISM = 8
const MAX_CHUNKS_ARBITRARY = 4000

func scrape(entry string, depth int) []common.Page {
	u, _ := url.Parse(entry)
	domain := u.Hostname()
	// Create a Collector
	c := colly.NewCollector(
		colly.AllowedDomains(domain),
		colly.Async(true),
		colly.MaxDepth(depth),
	)

	stream := make(chan pageChunk, SCRAPER_PARALLELISM)
	hasHitLimit := false

	// have the scraper run on multiple goroutines, which are abstractions
	// over threads
	c.Limit(&colly.LimitRule{DomainGlob: "*", Parallelism: SCRAPER_PARALLELISM})
	c.SetRequestTimeout(10 * time.Second)

	// Visit every link
	c.OnHTML("a[href]", func(e *colly.HTMLElement) {
		if !hasHitLimit {
			e.Request.Visit(e.Attr("href"))
			log.Output(1, "visit "+e.Attr("href"))
		}
	})

	hash_set := common.MakeThreadSafeHashSet()

	// Visit all content tags
	c.OnHTML("p, pre, h1, h2, h3, h4, h5, h6", func(e *colly.HTMLElement) {
		var chunk pageChunk

		switch e.Name {
		case "p":
			chunk = parseParagraph(e)
		case "pre":
			chunk = parsePre(e)
		default:
			chunk = parseHeader(e)
		}

		chunk.index = e.Index
		chunk.route = e.Request.URL.Path

		query := chunk.text + chunk.route

		if !hash_set.Has(query) {
			stream <- chunk
			hash_set.Add(query)
		}
	})

	c.OnError(func(r *colly.Response, err error) {
		fmt.Println("Request URL:", r.Request.URL, "failed with response:", r, "\nError:", err)
	})

	// make a map of string to array of strings
	chunk_map := make(map[string][]pageChunk)

	// start an infinite loop on another goroutine. This follows the map-reduce pattern.
	// every time a collector encounters a chunk, it is appended to the chunk_map indexed by
	// the correct route, culminating in one big map.
	go func() {
		chunksEncountered := 0
		for {
			select {
			case chunk := <-stream:
				chunksEncountered += 1
				if chunksEncountered < MAX_CHUNKS_ARBITRARY {
					chunk_map[chunk.route] = append(chunk_map[chunk.route], chunk)
				} else {
					hasHitLimit = true
				}
			}
		}
	}()

	// Visit a website
	c.Visit(entry)

	// wait for all scrapers to finish
	c.Wait()

	// purge the queye
	for len(stream) > 0 {
	}

	content := make([]common.Page, 0, len(chunk_map))
	// process pages
	numChunks := 0
	for k, v := range chunk_map {
		numChunks += len(v)
		page := processPage(v)
		page.Route = k
		content = append(content, page)
	}
	log.Output(1, fmt.Sprintf("parsed %d chunks", numChunks))

	return content
}

const BLOCK_FACTOR = 10
const EMBEDDING_MODEL = gogpt.AdaEmbeddingV2

func summarize(content []common.Page) []common.Page {
	ctx := context.Background()
	c := gogpt.NewClient(os.Getenv("OPENAI_API_KEY"))

	items := make([]string, 0)

	for _, v := range content {
		for _, v := range v.Sections {
			items = append(items, v.Zip())
		}
	}

	req := gogpt.EmbeddingRequest{
		Input: items,
		Model: gogpt.AdaEmbeddingV2,
	}
	res, err := c.CreateEmbeddings(ctx, req)

	if err != nil {
		panic(err)
	}

	k := 0
	for i, page := range content {
		for j, sec := range page.Sections {
			sec.Embedding = res.Data[k].Embedding
			page.Sections[j] = sec
			k += 1
		}
		// for indirection purposes
		content[i] = page
	}

	return content
}

func mongoUpload(domain common.Domain) {
	db, disconnect := common.GetDb()
	defer disconnect()
	coll := db.Collection("ScrapedDomains")

	// if the collection contains a document with the same domain.domain, replace it, otherwise add a new one
	_, err := coll.ReplaceOne(
		context.TODO(),
		bson.M{"domain": domain.Domain},
		domain,
		options.Replace().SetUpsert(true),
	)
	if err != nil {
		log.Fatal(err)
	}
}

type scrapeRequest struct {
	Domain string `json:"domain"`
	Depth  int    `json:"depth"`
}

type scrapeResponse struct {
	Success          bool   `json:"success"`
	Domain           string `json:"domain"`
	Error            string `json:"error"`
	ScrapedPageCount int    `json:"scraped_page_count"`
}

func Handler(w http.ResponseWriter, r *http.Request) {
	// deal with cors for local dev reasons
	w.Header().Set("Access-Control-Allow-Origin", "*")

	if r.Method == "POST" {
		var req scrapeRequest
		// parse from request body
		err := json.NewDecoder(r.Body).Decode(&req)
		if err != nil {
			panic(err.Error())
		}

		siteUrl := req.Domain
		siteUrl = strings.TrimSpace(siteUrl)

		// check if siteUrl is valid
		_, err = url.ParseRequestURI(siteUrl)
		if err != nil {
			panic(err.Error())
		}

		log.Output(1, "scraping "+siteUrl)
		content := scrape(siteUrl, req.Depth)
		log.Output(1, fmt.Sprintf("scraped %d pages from %s", len(content), siteUrl))

		sections := 0
		for _, v := range content {
			sections += len(v.Sections)
		}

		log.Output(1, fmt.Sprintf("generating %d embeddings for %s", sections, siteUrl))
		content = summarize(content)
		log.Output(1, fmt.Sprintf("generated embeddings for %s", siteUrl))

		// decode query-encoded domain. Must be done due to the idiosyncracies of browsers and JS
		parsed, err := url.Parse(req.Domain)
		if err != nil {
			panic(err)
		}
    encodedDomain := parsed.Host + parsed.Path

    log.Output(1, fmt.Sprintf("encoded %s as %s", req.Domain, encodedDomain))
		log.Output(1, fmt.Sprintf("uploading %s", encodedDomain))
		domain := common.Domain{
			Domain: encodedDomain,
			Pages:  content,
		}
    for _, v := range domain.Pages {
      v.Print()
    }
		mongoUpload(domain)
		log.Output(1, fmt.Sprintf("uploaded %s", encodedDomain))

		res := scrapeResponse{
			Success:          true,
			Domain:           req.Domain,
			ScrapedPageCount: len(content),
		}

		// send res as json
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(res)
	} else {
		// send an error
		w.WriteHeader(http.StatusMethodNotAllowed)
		w.Write([]byte("405 - Method Not Allowed"))
	}
}
