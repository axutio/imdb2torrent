package imdb2torrent

import (
	"context"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tidwall/gjson"
	"go.uber.org/zap"
)

type RARBGclientOptions struct {
	// Typically "https://torrentapi.org"
	BaseURL  string
	Timeout  time.Duration
	CacheAge time.Duration
}

func NewRARBGclientOpts(baseURL string, timeout, cacheAge time.Duration) RARBGclientOptions {
	return RARBGclientOptions{
		BaseURL:  baseURL,
		Timeout:  timeout,
		CacheAge: cacheAge,
	}
}

var DefaultRARBGclientOpts = RARBGclientOptions{
	BaseURL:  "https://torrentapi.org",
	Timeout:  5 * time.Second,
	CacheAge: 24 * time.Hour,
}

var _ MagnetSearcher = (*rarbgClient)(nil)

type rarbgClient struct {
	baseURL          string
	httpClient       *http.Client
	cache            Cache
	metaGetter		 MetaGetter
	cacheAge         time.Duration
	logger           *zap.Logger
	logFoundTorrents bool
	token            string
	tokenExpired     func() bool
	lastRequest      time.Time
	lock             *sync.Mutex
}

func NewRARBGclient(opts RARBGclientOptions, cache Cache, metaGetter MetaGetter, logger *zap.Logger, logFoundTorrents bool) *rarbgClient {
	return &rarbgClient{
		baseURL: opts.BaseURL,
		httpClient: &http.Client{
			Timeout: opts.Timeout,
		},
		cache:            cache,
		metaGetter:		  metaGetter,
		cacheAge:         opts.CacheAge,
		logger:           logger,
		logFoundTorrents: logFoundTorrents,
		tokenExpired:     func() bool { return true },
		lock:             &sync.Mutex{},
	}
}

// FindMovie uses RARBG's API to find torrents for the given IMDb ID.
// If no error occured, but there are just no torrents for the movie yet, an empty result and *no* error are returned.
func (c *rarbgClient) FindMovie(ctx context.Context, imdbID string) ([]Result, error) {
	// Get movie name
	meta, err := c.metaGetter.GetMovieSimple(ctx, imdbID)
	if err != nil {
		return nil, fmt.Errorf("Couldn't get movie name via Cinemeta for IMDb ID %v: %v", imdbID, err)
	}
	movieSearch := meta.Title
	if meta.Year != 0 {
		movieSearch += " " + strconv.Itoa(meta.Year)
	}
	movieSearch = url.PathEscape(movieSearch)

	escapedQuery := "search_string=" + movieSearch
	return c.find(ctx, imdbID, escapedQuery)
}

// FindTVShow uses RARBG's API to find torrents for the given IMDb ID + season + episode.
// If no error occured, but there are just no torrents for the TV show yet, an empty result and *no* error are returned.
func (c *rarbgClient) FindTVShow(ctx context.Context, imdbID string, season, episode int) ([]Result, error) {
	seasonString := strconv.Itoa(season)
	episodeString := strconv.Itoa(episode)
	id := imdbID + ":" + seasonString + ":" + episodeString
	// RARBG / torrentapi supports TV show search via IMDBb ID, even (and only) via the show's IMDb,
	// AND allows us to additionally filter by name, so we can filter for the season + episode here! Nice!
	// AM 210623 but this doesn't seem to work lol so let's just stick to using a search string
	// if season < 10 {
	// 	seasonString = "0" + seasonString
	// }
	// if episode < 10 {
	// 	episodeString = "0" + episodeString
	// }
	// escapedQuery := "search_imdb=" + imdbID + "&search_string=S" + seasonString + "E" + episodeString

	tvShowSearch, err := createTVShowSearch(ctx, c.metaGetter, imdbID, season, episode)
	if err != nil {
		return nil, err
	}
	tvShowSearch = url.PathEscape(tvShowSearch)
	escapedQuery := "search_string=" + tvShowSearch;

	return c.find(ctx, id, escapedQuery)
}

// Query must be URL-escaped already.
func (c *rarbgClient) find(ctx context.Context, id, escapedQuery string) ([]Result, error) {
	zapFieldID := zap.String("id", id)
	zapFieldTorrentSite := zap.String("torrentSite", "RARBG")

	// Check cache first
	cacheKey := id + "-RARBG"
	torrentList, created, found, err := c.cache.Get(cacheKey)
	if err != nil {
		c.logger.Error("Couldn't get torrent results from cache", zap.Error(err), zapFieldID, zapFieldTorrentSite)
	} else if !found {
		c.logger.Debug("Torrent results not found in cache", zapFieldID, zapFieldTorrentSite)
	} else if time.Since(created) > (c.cacheAge) {
		expiredSince := time.Since(created.Add(c.cacheAge))
		c.logger.Debug("Hit cache for torrents, but item is expired", zap.Duration("expiredSince", expiredSince), zapFieldID, zapFieldTorrentSite)
	} else {
		c.logger.Debug("Hit cache for torrents, returning results", zap.Int("torrentCount", len(torrentList)), zapFieldID, zapFieldTorrentSite)
		return torrentList, nil
	}

	// Check token expiration
	if c.tokenExpired() {
		if err = c.RefreshToken(); err != nil {
			c.logger.Error("Couldn't refresh token", zap.Error(err), zapFieldID, zapFieldTorrentSite)
			return nil, nil
		}
	}

	// Prevent concurrent requests *and* wait for 2 seconds to pass if necessary, so we don't hit the rate limit
	c.lock.Lock()
	time.Sleep(2*time.Second - time.Since(c.lastRequest))
	defer func() {
		c.lock.Unlock()
		c.lastRequest = time.Now()
	}()

	// `format=json_extended` for size info
	url := c.baseURL + "/pubapi_v2.php?app_id=deflix&mode=search&sort=seeders&format=json_extended&ranked=0&token=" + c.token + "&" + escapedQuery
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("Couldn't create request: %v", err)
	}
	// For one and the same IMDb ID, RARBG sometimes returns results, sometimes a JSON saying that no result was found.
	// The rate of success is higher with cURL, and very low when doing `c.httpClient.Get(url)`, so we're trying to identify as cURL.
	req.Header.Set("User-Agent", "curl/7.47.0")
	req.Header.Set("Accept", "*/*")
	res, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("Couldn't GET %v: %v", url, err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Bad GET response: %v", res.StatusCode)
	}
	resBody, err := ioutil.ReadAll(res.Body)
	if err != nil {
		return nil, fmt.Errorf("Couldn't read response body: %v", err)
	}

	// Extract data from JSON
	torrents := gjson.GetBytes(resBody, "torrent_results").Array()
	if len(torrents) == 0 {
		// Nil slice is ok, because it can be checked with len()
		return nil, nil
	}
	var results []Result
	for _, torrent := range torrents {
		// Note: When using `format=json` or no format in the query, use "filename" here,
		// otherwise (`format=json_extended`) use "title" here.
		filename := torrent.Get("title").String()

		quality := ""
		if strings.Contains(filename, "720p") {
			quality = "720p"
		} else if strings.Contains(filename, "1080p") {
			quality = "1080p"
		} else if strings.Contains(filename, "2160p") {
			quality = "2160p"
		} else {
			continue
		}

		magnet := torrent.Get("download").String()

		// look for "btih:dd8255ecdc7ca55fb0bbf81323d87062db1f6d1c&" via regex and then cut out the hash
		match := magnet2InfoHashRegex.Find([]byte(magnet))
		infoHash := strings.TrimPrefix(string(match), "btih:")
		infoHash = strings.TrimSuffix(infoHash, "&")
		infoHash = strings.ToLower(infoHash)
		if len(infoHash) != 40 {
			c.logger.Error("InfoHash isn't 40 characters long", zap.String("magnet", magnet), zapFieldID, zapFieldTorrentSite)
			continue
		}
		size := int(torrent.Get("size").Int())
		seeders := int(torrent.Get("seeders").Int())

		if c.logFoundTorrents {
			c.logger.Debug("Found torrent", zap.String("quality", quality), zap.String("infoHash", infoHash), zap.String("magnet", magnet), zap.Int("size", size), zap.Int("seeders", seeders), zapFieldID, zapFieldTorrentSite)
		}
		result := Result{
			Name: filename,
			// We don't know the title, but it will be overwritten by the quality anyway
			// Title: "",
			Quality:   quality,
			InfoHash:  infoHash,
			MagnetURL: magnet,
			Size:      size,
			Seeders:   seeders,
		}
		results = append(results, result)
	}

	// Fill cache, even if there are no results, because that's just the current state of the torrent site.
	// Any actual errors would have returned earlier.
	if err := c.cache.Set(cacheKey, results); err != nil {
		c.logger.Error("Couldn't cache torrents", zap.Error(err), zap.String("cache", "torrent"), zapFieldID, zapFieldTorrentSite)
	}

	return results, nil
}

func (c *rarbgClient) IsSlow() bool {
	return true
}

func (c *rarbgClient) RefreshToken() error {
	url := c.baseURL + "/pubapi_v2.php?app_id=deflix&get_token=get_token"
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return fmt.Errorf("Couldn't create request object: %v", req)
	}

	// Prevent concurrent requests *and* wait for 2 seconds to pass if necessary, so we don't hit the rate limit
	c.lock.Lock()
	time.Sleep(2*time.Second - time.Since(c.lastRequest))
	defer func() {
		c.lock.Unlock()
		c.lastRequest = time.Now()
	}()
	// After getting the lock, check expiry again (was already checked before RefreshToken() was called) to not send this request several times due to concurrent incoming requests after the token expired.
	if !c.tokenExpired() {
		return nil
	}

	res, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("Couldn't GET %v: %v", url, err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("Bad GET response: %v", res.StatusCode)
	}
	resBody, err := ioutil.ReadAll(res.Body)
	if err != nil {
		return fmt.Errorf("Couldn't read response body: %v", err)
	}
	token := gjson.GetBytes(resBody, "token").String()
	if token == "" {
		return fmt.Errorf("Token is empty")
	}
	c.token = token
	createdAt := time.Now()
	c.tokenExpired = func() bool {
		return time.Since(createdAt).Minutes() > 14
	}
	return nil
}
