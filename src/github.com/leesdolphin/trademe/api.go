package main

import (
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/net/html"
)

type badSearchPage struct{}

func (p badSearchPage) Error() string {
	return "badSearchPage"
}

type propertyData struct {
	listingId          string
	uri                string
	title, description string
	otherData          map[string]string
	price              float64
	images             []string
	locationData       propertyLocationData
}

type propertyLocationData struct {
	lat, long      float64
	street, suburb string
}

var propertyURLRegex = regexp.MustCompile("http[s]?:\\/\\/www\\.trademe\\.co\\.nz\\/property\\/residential-property[a-z\\-]+?/auction-\\d+.htm")

func multiplex(multiplexCount int, execFunc func(chan *propertyData)) chan *propertyData {
	// See: https://blog.golang.org/pipelines#TOC_4.
	outChan := make(chan *propertyData, 2)
	var wg sync.WaitGroup
	wg.Add(multiplexCount)
	fn := func() {
		execFunc(outChan)
		wg.Done()
	}
	for i := 0; i < multiplexCount; i++ {
		go fn()
	}
	go func() {
		wg.Wait()
		close(outChan)
	}()
	return outChan
}

func loadPropertyFromSeedURLs(seedUrls []string, propertyUrls chan *url.URL) error {
	searchResultsChan := make(chan *url.URL, 100)
	searchPageChan := make(chan *url.URL, len(seedUrls)+5)
	for _, urlStr := range seedUrls {
		uri, err := url.Parse(urlStr)
		if err != nil {
			return err
		}
		searchPageChan <- uri
	}
	go func() {
		searchPageUrls := make(map[string]bool)
		for {
			select {
			case searchUrl := <-searchPageChan:
				if searchUrl == nil {
					continue
				}
				exists := searchPageUrls[searchUrl.String()] // If missing returns false.
				if exists {
					continue
				}
				searchPageUrls[searchUrl.String()] = true
				fmt.Println(searchUrl)
				loadPropertyFromURLList(searchUrl, searchPageChan, searchResultsChan)
			default: // Run out of pages. Die.
				// No more to add to results.
				close(searchResultsChan)
				close(searchPageChan)
				break
			}
		}
	}()
	go func() {
		searchResultsUrls := make(map[string]bool)
		for propertyUrl := range searchResultsChan {
			exists := searchResultsUrls[propertyUrl.String()] // If missing returns false.
			if exists {
				continue
			}
			searchResultsUrls[propertyUrl.String()] = true
			propertyUrls <- propertyUrl
		}
		close(propertyUrls)
	}()
	return nil
}

func main() {
	// fmt.Println("Starting")
	seedURLs := []string{}
	seedURLs = append(seedURLs,
		"http://www.trademe.co.nz/Browse/CategoryAttributeSearchResults.aspx?search=1&cid=5748&sidebar=1&rptpath=350-5748-4233-&132=FLAT&selected135=7&134=1&135=7&216=0&216=0&217=0&217=0&153=&122=0&122=0&59=20000&59=40000&178=0&178=0&sidebarSearch_keypresses=0&sidebarSearch_suggested=0")
	propertyURLs := make(chan *url.URL)
	err := loadPropertyFromSeedURLs(seedURLs, propertyURLs)
	if err != nil {
		fmt.Println(err)
		return
	}

	for uri := range propertyURLs {
		fmt.Println(loadPropertyDataFrom(uri))
	}
}

func loadPropertyFromURLList(baseURL *url.URL, searchPageChan, searchResultsChan chan *url.URL) error {
	resp, err := http.Get(baseURL.String())
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	z := html.NewTokenizer(resp.Body)
	for {
		tt := z.Next()
		if tt == html.ErrorToken {
			// Probably the end of the file ...
			fmt.Println("Error: ", z.Err())
			break
		}
		if tt == html.StartTagToken {
			tagName := getTagName(z)
			switch tagName {
			case "a":
				attrMap := getAttrs(z)
				href, ok := attrMap["href"]
				if ok {
					tagURL, err := getPropertyURL(baseURL, href)
					if err == nil && tagURL != nil {
						searchResultsChan <- tagURL
					} else if rel, ok := attrMap["rel"]; ok && rel == "next" {
						uri, err := getURLRel(baseURL, href)
						if err == nil {
							searchPageChan <- uri
						}
					}
				}
			case "div":
				attrMap := getAttrs(z)
				// Handle error page.
				id, ok := attrMap["id"]
				if ok && id == "ErrorOops" {
					return badSearchPage{}
				}
			}
		}
	}
	fmt.Println("Done loading search results")
	return nil
}

func getTagName(z *html.Tokenizer) string {
	nameB, _ := z.TagName()
	return string(nameB)
}
func ifTag(z *html.Tokenizer, tagName string) bool {
	nameB, _ := z.TagName()
	return string(nameB) == tagName
}
func getURLRel(baseURL *url.URL, hrefValue string) (*url.URL, error) {
	tagURL, err := url.Parse(hrefValue)
	if err == nil {
		uri := baseURL.ResolveReference(tagURL)
		return uri, nil
	}
	return nil, err
}

func getPropertyURL(baseURL *url.URL, hrefValue string) (*url.URL, error) {
	uri, err := getURLRel(baseURL, hrefValue)
	if err == nil {
		if propertyURLRegex.MatchString(uri.String()) {
			return uri, nil
		}
		return nil, nil
	}
	return nil, err
}

func getAttrs(z *html.Tokenizer) map[string]string {
	attrMap := make(map[string]string, 10)
	kB, vB, _ := z.TagAttr()
	for len(kB) > 0 {
		k, v := string(kB), string(vB)
		attrMap[k] = v
		kB, vB, _ = z.TagAttr()
	}
	return attrMap
}
func loadPropertyDataFrom(propertyURL *url.URL) *propertyData {
	resp, err := http.Get(propertyURL.String())
	if err != nil {
		fmt.Println("Error getting", err)
		return nil
	}
	defer resp.Body.Close()
	z := html.NewTokenizer(resp.Body)
	if err := findTagWithAttr(z, "div", "id", "mainContent"); err == nil {
		return loadDataFromMainContent(propertyURL, z)
	} else {
		fmt.Println("Error getting", err, z.Err())
	}
	return nil
}
func findTagWithAttr(z *html.Tokenizer, tagName, attrName, attrValue string) error {
	for {
		tt := z.Next()
		if tt == html.ErrorToken {
			return z.Err()
		} else if tt == html.StartTagToken {
			currTagName := getTagName(z)
			attrMap := getAttrs(z)
			if currTagName == tagName {
				currAttrValue, ok := attrMap[attrName]
				if ok && currAttrValue == attrValue {
					return nil
				}
			}
		}
	}
}
func readText(z *html.Tokenizer) (string, error) {
	text := ""
	for {
		tt := z.Next()
		switch tt {
		case html.ErrorToken:
			return "", z.Err()
		case html.TextToken:
			text = text + string(z.Text())
		case html.SelfClosingTagToken:
			switch getTagName(z) {
			case "br":
				text = text + "\n"
				continue
			}
			fallthrough
		default:
			return strings.Trim(text, " \n\t"), nil
		}
	}
}
func readTextFromTagWithAttr(z *html.Tokenizer, tagName, attrName, attrValue string) (string, error) {
	err := findTagWithAttr(z, tagName, attrName, attrValue)
	if err != nil {
		return "", err
	}
	text, err := readText(z)
	if err != nil {
		return "", err
	}
	return text, nil
}
func loadDataFromMainContent(uri *url.URL, z *html.Tokenizer) *propertyData {
	data := new(propertyData)
	data.uri = uri.String()
	re := regexp.MustCompile(`\$(\d+(?:\.\d{2})?)`) // Matches $(123(.45)?)
	fmt.Println("Loading ", uri)
	// Find <h1 id="ListingTitle_title">
	text, err := readTextFromTagWithAttr(z, "h1", "id", "ListingTitle_title")
	if err != nil {
		fmt.Println("Title", err)
		return nil
	}
	data.title = text

	text, err = readTextFromTagWithAttr(z, "li", "id", "ListingTitle_classifiedTitlePrice")
	if err != nil {
		fmt.Println("Price", err)
		return nil
	}
	matches := re.FindStringSubmatch(text)
	if len(matches) != 2 {
		fmt.Println("Regex", matches, text)
		return nil
	}
	priceF, err := strconv.ParseFloat(matches[1], 64)
	if err != nil {
		fmt.Println("Float Parse", err, matches)
		return nil
	}
	data.price = priceF

	findTagWithAttr(z, "ul", "id", "thumbs")
	err = readThumbnails(uri, z, data)
	if err != nil {
		fmt.Println("Thumbnails", err)
		return nil
	}

	findTagWithAttr(z, "table", "id", "ListingAttributes")
	err = readListAttrsTable(uri, z, data)
	if err != nil {
		fmt.Println("Attrs", err)
		return nil
	}

	data.description, err = readTextFromTagWithAttr(z, "div", "id", "ListingDescription_ListingDescription")

	findTagWithAttr(z, "script", "id", "info-tooltip-tmpl")
	mapScriptContent, err := readTextFromTagWithAttr(z, "script", "type", "text/javascript")

	fmt.Println(mapScriptContent)

	fmt.Println(data)
	return data
}
func readThumbnails(baseURL *url.URL, z *html.Tokenizer, data *propertyData) error {
	thumbUrlRegex := regexp.MustCompile("/thumb/")
	thumbnails := make(map[string]bool)
	for {
		tt := z.Next()
		if tt == html.ErrorToken {
			// Probably the end of the file ...
			fmt.Println("Error: ", z.Err())
			return z.Err()
		} else if tt == html.EndTagToken || tt == html.StartTagToken || tt == html.SelfClosingTagToken {
			tagName := getTagName(z)
			fmt.Println(tt, " -- ", tagName)
			switch tagName {
			case "ul":
				size := len(thumbnails)
				data.images = make([]string, size)
				for imgUrl, _ := range thumbnails {
					data.images = append(data.images, imgUrl)
				}
				return nil
			case "img":
				attrMap := getAttrs(z)
				src, ok := attrMap["src"]
				if ok && thumbUrlRegex.MatchString(src) {
					fullSrc := thumbUrlRegex.ReplaceAllLiteralString(src, "/full/")
					imgUrl, err := getURLRel(baseURL, fullSrc)
					if err == nil && imgUrl != nil {
						thumbnails[imgUrl.String()] = true
					}
				}
			}
		}
	}
}

func readListAttrsTable(baseURL *url.URL, z *html.Tokenizer, data *propertyData) error {
	currKey := ""
	err := z.Err() // Init err.
	attrsMap := make(map[string]string)
	for {
		tt := z.Next()
		switch tt {
		case html.ErrorToken:
			return z.Err()
		case html.StartTagToken:
			tagName := getTagName(z)
			switch tagName {
			case "th":
				currKey, err = readText(z)
				if err != nil {
					return err
				}
			case "td":
				value, err := readText(z)
				if err != nil {
					return err
				}
				attrsMap[currKey] = value
			}
		case html.EndTagToken:
			tagName := getTagName(z)
			if tagName == "table" {
				data.otherData = attrsMap
				return nil
			}
		default:
		}
	}
}
