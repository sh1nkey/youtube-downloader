package youtube

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

const (
	Size1Kb  = 1024
	Size1Mb  = Size1Kb * 1024
	Size10Mb = Size1Mb * 10

	playerParams = "CgIQBg=="
)

const ContentPlaybackNonceAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"

var ErrNoFormat = errors.New("no video format provided")

// DefaultClient type to use. No reason to change but you could if you wanted to.
var DefaultClient = IOSClient

// Client offers methods to download video metadata and video streams.
type Client struct {
	// HTTPClient can be used to set a custom HTTP client.
	// If not set, http.DefaultClient will be used
	HTTPClient *http.Client

	// MaxRoutines to use when downloading a video.
	MaxRoutines int

	// ChunkSize to use when downloading videos in chunks. Default is Size10Mb.
	ChunkSize int64

	// playerCache caches the JavaScript code of a player response
	playerCache playerCache

	client *clientInfo

	consentID string

	visitorId struct {
		value   string
		updated time.Time
	}
}

func (c *Client) assureClient() {
	if c.client == nil {
		c.client = &DefaultClient
	}
}

// GetVideo fetches video metadata
func (c *Client) GetVideo(url string) (*Video, error) {
	return c.GetVideoContext(context.Background(), url)
}

// GetVideoContext fetches video metadata with a context
func (c *Client) GetVideoContext(ctx context.Context, url string) (*Video, error) {
	id, err := ExtractVideoID(url)
	if err != nil {
		return nil, fmt.Errorf("extractVideoID failed: %w", err)
	}

	return c.videoFromID(ctx, id)
}

func (c *Client) videoFromID(ctx context.Context, id string) (*Video, error) {
	c.assureClient()

	body, err := c.videoDataByInnertube(ctx, id)
	if err != nil {
		return nil, err
	}

	v := Video{
		ID: id,
	}

	// return early if all good
	if err = v.parseVideoInfo(body); err == nil {
		return &v, nil
	}

	// If the uploader has disabled embedding the video on other sites, parse video page
	if errors.Is(err, ErrNotPlayableInEmbed) {
		// additional parameters are required to access clips with sensitiv content
		html, err := c.httpGetBodyBytes(ctx, "https://www.youtube.com/watch?v="+id+"&bpctr=9999999999&has_verified=1")
		if err != nil {
			return nil, err
		}

		return &v, v.parseVideoPage(html)
	}

	// If the uploader marked the video as inappropriate for some ages, use embed player
	if errors.Is(err, ErrLoginRequired) {
		c.client = &EmbeddedClient

		bodyEmbed, errEmbed := c.videoDataByInnertube(ctx, id)
		if errEmbed == nil {
			errEmbed = v.parseVideoInfo(bodyEmbed)
		}

		if errEmbed == nil {
			return &v, nil
		}

		// private video clearly not age-restricted and thus should be explicit
		if errEmbed == ErrVideoPrivate {
			return &v, errEmbed
		}

		// wrapping error so its clear whats happened
		return &v, fmt.Errorf("can't bypass age restriction: %w", errEmbed)
	}

	// undefined error
	return &v, err
}

type innertubeRequest struct {
	VideoID         string            `json:"videoId,omitempty"`
	BrowseID        string            `json:"browseId,omitempty"`
	Continuation    string            `json:"continuation,omitempty"`
	Context         inntertubeContext `json:"context"`
	PlaybackContext *playbackContext  `json:"playbackContext,omitempty"`
	ContentCheckOK  bool              `json:"contentCheckOk,omitempty"`
	RacyCheckOk     bool              `json:"racyCheckOk,omitempty"`
	Params          string            `json:"params"`
}

type playbackContext struct {
	ContentPlaybackContext contentPlaybackContext `json:"contentPlaybackContext"`
}

type contentPlaybackContext struct {
	// SignatureTimestamp string `json:"signatureTimestamp"`
	HTML5Preference string `json:"html5Preference"`
}

type inntertubeContext struct {
	Client innertubeClient `json:"client"`
}

type innertubeClient struct {
	HL                string `json:"hl"`
	GL                string `json:"gl"`
	ClientName        string `json:"clientName"`
	ClientVersion     string `json:"clientVersion"`
	AndroidSDKVersion int    `json:"androidSDKVersion,omitempty"`
	UserAgent         string `json:"userAgent,omitempty"`
	TimeZone          string `json:"timeZone"`
	UTCOffset         int    `json:"utcOffsetMinutes"`
	DeviceModel       string `json:"deviceModel,omitempty"`
	VisitorData       string `json:"visitorData,omitempty"`
}

// client info for the innertube API
type clientInfo struct {
	name           string
	key            string
	version        string
	userAgent      string
	androidVersion int
	deviceModel    string
}

var (
	// WebClient, better to use Android client but go ahead.
	WebClient = clientInfo{
		name:      "WEB",
		version:   "2.20220801.00.00",
		key:       "AIzaSyAO_FJ2SlqU8Q4STEHLGCilw_Y9_11qcW8",
		userAgent: "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36",
	}

	// AndroidClient, download go brrrrrr.
	AndroidClient = clientInfo{
		name:           "ANDROID",
		version:        "18.11.34",
		key:            "AIzaSyA8eiZmM1FaDVjRy-df2KTyQ_vz_yYM39w",
		userAgent:      "com.google.android.youtube/18.11.34 (Linux; U; Android 11) gzip",
		androidVersion: 30,
	}

	// IOSClient Client based brrrr.
	IOSClient = clientInfo{
		name:        "IOS",
		version:     "19.45.4",
		key:         "AIzaSyAO_FJ2SlqU8Q4STEHLGCilw_Y9_11qcW8",
		userAgent:   "com.google.ios.youtube/19.45.4 (iPhone16,2; U; CPU iOS 18_1_0 like Mac OS X;)",
		deviceModel: "iPhone16,2",
	}

	// EmbeddedClient, not really tested.
	EmbeddedClient = clientInfo{
		name:      "WEB_EMBEDDED_PLAYER",
		version:   "1.19700101",
		key:       "AIzaSyAO_FJ2SlqU8Q4STEHLGCilw_Y9_11qcW8", // seems like same key works for both clients
		userAgent: "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36",
	}
)

func (c *Client) videoDataByInnertube(ctx context.Context, id string) ([]byte, error) {
	data := innertubeRequest{
		VideoID:        id,
		Context:        prepareInnertubeContext(*c.client),
		ContentCheckOK: true,
		RacyCheckOk:    true,
		// Params:                   playerParams,
		PlaybackContext: &playbackContext{
			ContentPlaybackContext: contentPlaybackContext{
				// SignatureTimestamp: sts,
				HTML5Preference: "HTML5_PREF_WANTS",
			},
		},
	}

	return c.httpPostBodyBytes(ctx, "https://www.youtube.com/youtubei/v1/player?key="+c.client.key, data)
}

func (c *Client) transcriptDataByInnertube(ctx context.Context, id string, lang string) ([]byte, error) {
	data := innertubeRequest{
		Context: prepareInnertubeContext(*c.client),
		Params:  transcriptVideoID(id, lang),
	}

	return c.httpPostBodyBytes(ctx, "https://www.youtube.com/youtubei/v1/get_transcript?key="+c.client.key, data)
}

func randString(alphabet string, sz int) string {
	var buf strings.Builder
	buf.Grow(sz)
	for i := 0; i < sz; i++ {
		buf.WriteByte(alphabet[rand.Intn(len(alphabet))])
	}
	return buf.String()
}

func randomVisitorData(countryCode string) string {
	var pbE2 ProtoBuilder

	pbE2.String(2, "")
	pbE2.Varint(4, int64(rand.Intn(255)+1))

	var pbE ProtoBuilder
	pbE.String(1, countryCode)
	pbE.Bytes(2, pbE2.ToBytes())

	var pb ProtoBuilder
	pb.String(1, randString(ContentPlaybackNonceAlphabet, 11))
	pb.Varint(5, time.Now().Unix()-int64(rand.Intn(600000)))
	pb.Bytes(6, pbE.ToBytes())

	return pb.ToURLEncodedBase64()
}

func prepareInnertubeContext(clientInfo clientInfo) inntertubeContext {
	return inntertubeContext{
		Client: innertubeClient{
			HL:                "en",
			GL:                "US",
			TimeZone:          "UTC",
			DeviceModel:       clientInfo.deviceModel,
			ClientName:        clientInfo.name,
			ClientVersion:     clientInfo.version,
			AndroidSDKVersion: clientInfo.androidVersion,
			UserAgent:         clientInfo.userAgent,
			VisitorData:       randomVisitorData("US"),
		},
	}
}

func prepareInnertubePlaylistData(ID string, continuation bool, clientInfo clientInfo) innertubeRequest {
	context := prepareInnertubeContext(clientInfo)

	if continuation {
		return innertubeRequest{
			Context:        context,
			Continuation:   ID,
			ContentCheckOK: true,
			RacyCheckOk:    true,
			Params:         playerParams,
		}
	}

	return innertubeRequest{
		Context:        context,
		BrowseID:       "VL" + ID,
		ContentCheckOK: true,
		RacyCheckOk:    true,
		Params:         playerParams,
	}
}

// transcriptVideoID encodes the video ID to the param used to fetch transcripts.
func transcriptVideoID(videoID string, lang string) string {
	langCode := encTranscriptLang(lang)

	// This can be optionally appened to the Sprintf str, not sure what it means
	// *3engagement-panel-searchable-transcript-search-panel\x30\x00\x38\x01\x40\x01
	return base64Enc(fmt.Sprintf("\n\x0b%s\x12\x12%s\x18\x01", videoID, langCode))
}

func encTranscriptLang(languageCode string) string {
	s := fmt.Sprintf("\n\x03asr\x12\x02%s\x1a\x00", languageCode)
	s = base64PadEnc(s)

	return url.QueryEscape(s)
}

// GetPlaylist fetches playlist metadata
func (c *Client) GetPlaylist(url string) (*Playlist, error) {
	return c.GetPlaylistContext(context.Background(), url)
}

// GetPlaylistContext fetches playlist metadata, with a context, along with a list of Videos, and some basic information
// for these videos. Playlist entries cannot be downloaded, as they lack all the required metadata, but
// can be used to enumerate all IDs, Authors, Titles, etc.
func (c *Client) GetPlaylistContext(ctx context.Context, url string) (*Playlist, error) {
	c.assureClient()

	id, err := extractPlaylistID(url)
	if err != nil {
		return nil, fmt.Errorf("extractPlaylistID failed: %w", err)
	}

	data := prepareInnertubePlaylistData(id, false, *c.client)
	body, err := c.httpPostBodyBytes(ctx, "https://www.youtube.com/youtubei/v1/browse?key="+c.client.key, data)
	if err != nil {
		return nil, err
	}

	p := &Playlist{ID: id}
	return p, p.parsePlaylistInfo(ctx, c, body)
}

func (c *Client) VideoFromPlaylistEntry(entry *PlaylistEntry) (*Video, error) {
	return c.videoFromID(context.Background(), entry.ID)
}

func (c *Client) VideoFromPlaylistEntryContext(ctx context.Context, entry *PlaylistEntry) (*Video, error) {
	return c.videoFromID(ctx, entry.ID)
}

// GetStream returns the stream and the total size for a specific format
func (c *Client) GetStream(video *Video, format *Format, senderChan chan<- audioData) (int64, error) {
	return c.GetStreamContext(context.Background(), video, format, senderChan)
}

// GetStreamContext returns the stream and the total size for a specific format with a context.
func (c *Client) GetStreamContext(ctx context.Context, video *Video, format *Format, senderChan chan<- audioData) (int64, error) {
	url, err := c.GetStreamURL(video, format)
	if err != nil {
		return 0, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}


	contentLength := format.ContentLength

	if contentLength == 0 {
		// some videos don't have length information
		contentLength = c.downloadOnce(req,  senderChan)
	} else {
		// we have length information, let's download by chunks!
		c.downloadChunked(ctx, req, format, senderChan)
	}

	return contentLength, nil
}

func (c *Client) downloadOnce(req *http.Request,  senderChan chan<- audioData) int64 {
	resp, err := c.httpDo(req)
	if err != nil {

		return 0
	}

	go func() {
		defer resp.Body.Close()
		data, err := io.ReadAll(resp.Body)
		if err != nil {
			slog.Error("Failed to read response body", "error", err)
			return
		}
		senderChan <- audioData{
			data:        data,
			chunkNum:   1,
			totalChunks: 1,
		}
	}()

	contentLength := resp.Header.Get("Content-Length")
	length, _ := strconv.ParseInt(contentLength, 10, 64)

	return length
}

func (c *Client) getChunkSize() int64 {
	if c.ChunkSize > 0 {
		return c.ChunkSize
	}

	return Size10Mb
}

func (c *Client) getMaxRoutines(limit int) int {
	routines := 10

	if c.MaxRoutines > 0 {
		routines = c.MaxRoutines
	}

	if limit > 0 && routines > limit {
		routines = limit
	}

	return routines
}

func (c *Client) downloadChunked(ctx context.Context, req *http.Request, format *Format, senderChan chan<- audioData) {
	chunks := getChunks(format.ContentLength, c.getChunkSize())
	maxRoutines := c.getMaxRoutines(len(chunks))

	cancelCtx, cancel := context.WithCancel(ctx)
	abort := func(err error) {
		cancel()
	}

	totalChunks := len(chunks)

	currentChunk := atomic.Uint32{}
	for i := 0; i < maxRoutines; i++ {
		go func() {
			for {
				chunkIndex := int(currentChunk.Add(1)) - 1
				if chunkIndex >= len(chunks) {
					// no more chunks
					return
				}

				chunk := &chunks[chunkIndex]
				err := c.downloadChunk(req.Clone(cancelCtx), chunk, uint64(totalChunks))
				close(chunk.data)

				if err != nil {
					abort(err)
					return
				}
			}
		}()
	}

	go func() {
		// copy chunks into the PipeWriter
		for i := 0; i < len(chunks); i++ {
			select {
			case <-cancelCtx.Done():
				abort(context.Canceled)
				return
			case data := <-chunks[i].data:
				senderChan <- data
			}
		}

		// everything succeeded
	}()
}

// GetStreamURL returns the url for a specific format
func (c *Client) GetStreamURL(video *Video, format *Format) (string, error) {
	return c.GetStreamURLContext(context.Background(), video, format)
}

// GetStreamURLContext returns the url for a specific format with a context
func (c *Client) GetStreamURLContext(ctx context.Context, video *Video, format *Format) (string, error) {
	if format == nil {
		return "", ErrNoFormat
	}

	c.assureClient()

	if format.URL != "" {
		if c.client.androidVersion > 0 {
			return format.URL, nil
		}

		return c.unThrottle(ctx, video.ID, format.URL)
	}

	// TODO: check rest of this function, is it redundant?

	cipher := format.Cipher
	if cipher == "" {
		return "", ErrCipherNotFound
	}

	uri, err := c.decipherURL(ctx, video.ID, cipher)
	if err != nil {
		return "", err
	}

	return uri, err
}

// httpDo sends an HTTP request and returns an HTTP response.
func (c *Client) httpDo(req *http.Request) (*http.Response, error) {
	client := c.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}

	req.Header.Set("User-Agent", c.client.userAgent)
	req.Header.Set("Origin", "https://youtube.com")
	req.Header.Set("Sec-Fetch-Mode", "navigate")

	if len(c.consentID) == 0 {
		c.consentID = strconv.Itoa(rand.Intn(899) + 100) //nolint:gosec
	}

	req.AddCookie(&http.Cookie{
		Name:   "CONSENT",
		Value:  "YES+cb.20210328-17-p0.en+FX+" + c.consentID,
		Path:   "/",
		Domain: ".youtube.com",
	})

	res, err := client.Do(req)

	log := slog.With("method", req.Method, "url", req.URL)

	if err == nil && res.StatusCode != http.StatusOK {
		err = ErrUnexpectedStatusCode(res.StatusCode)
		res.Body.Close()
		res = nil
	}

	if err != nil {
		log.Debug("HTTP request failed", "error", err)
	} else {
		log.Debug("HTTP request succeeded", "status", res.Status)
	}

	return res, err
}

// httpGet does a HTTP GET request, checks the response to be a 200 OK and returns it
func (c *Client) httpGet(ctx context.Context, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}

	resp, err := c.httpDo(req)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, ErrUnexpectedStatusCode(resp.StatusCode)
	}

	return resp, nil
}

// httpGetBodyBytes reads the whole HTTP body and returns it
func (c *Client) httpGetBodyBytes(ctx context.Context, url string) ([]byte, error) {
	resp, err := c.httpGet(ctx, url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	return io.ReadAll(resp.Body)
}

// httpPost does a HTTP POST request with a body, checks the response to be a 200 OK and returns it
func (c *Client) httpPost(ctx context.Context, url string, body interface{}) (*http.Response, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}

	req.Header.Set("X-Youtube-Client-Name", "3")
	req.Header.Set("X-Youtube-Client-Version", c.client.version)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")

	if xgoogvisitorid, err := c.getVisitorId(); err != nil {
		return nil, err
	} else {
		req.Header.Set("x-goog-visitor-id", xgoogvisitorid)
	}

	resp, err := c.httpDo(req)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, ErrUnexpectedStatusCode(resp.StatusCode)
	}

	return resp, nil
}

var VisitorIdMaxAge = 10 * time.Hour

func (c *Client) getVisitorId() (string, error) {
	var err error
	if c.visitorId.value == "" || time.Since(c.visitorId.updated) > VisitorIdMaxAge {
		err = c.refreshVisitorId()
	}

	return c.visitorId.value, err
}

func (c *Client) refreshVisitorId() error {
	const sep = "\nytcfg.set("

	req, err := http.NewRequest(http.MethodGet, "https://www.youtube.com", nil)
	if err != nil {
		return err
	}

	resp, err := c.httpDo(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	_, data1, found := strings.Cut(string(data), sep)
	if !found {
		return err
	}
	var value struct {
		InnertubeContext struct {
			Client struct {
				VisitorData string
			}
		} `json:"INNERTUBE_CONTEXT"`
	}
	if err := json.NewDecoder(strings.NewReader(data1)).Decode(&value); err != nil {
		return err
	}

	if c.visitorId.value, err = url.PathUnescape(value.InnertubeContext.Client.VisitorData); err != nil {
		return err
	}

	c.visitorId.updated = time.Now()
	return nil
}

// httpPostBodyBytes reads the whole HTTP body and returns it
func (c *Client) httpPostBodyBytes(ctx context.Context, url string, body interface{}) ([]byte, error) {
	resp, err := c.httpPost(ctx, url, body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	return io.ReadAll(resp.Body)
}

// downloadChunk writes the response data into the data channel of the chunk.
// Downloading in multiple chunks is much faster:
// https://github.com/kkdai/youtube/pull/190
func (c *Client) downloadChunk(req *http.Request, chunk *chunk, totalChunks uint64) error {
	q := req.URL.Query()
	q.Set("range", fmt.Sprintf("%d-%d", chunk.start, chunk.end))
	req.URL.RawQuery = q.Encode()

	resp, err := c.httpDo(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return ErrUnexpectedStatusCode(resp.StatusCode)
	}

	expected := int(chunk.end-chunk.start) + 1
	data, err := io.ReadAll(resp.Body)
	n := len(data)

	if err != nil {
		return err
	}

	if n != expected {
		return fmt.Errorf("chunk at offset %d has invalid size: expected=%d actual=%d", chunk.start, expected, n)
	}

	chunk.data <- audioData{
		data:        data,
		chunkNum:   chunk.num,
		totalChunks: totalChunks,
	}

	return nil
}
