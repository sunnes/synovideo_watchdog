package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"image"
	"image/png"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gen2brain/h265/hevc"
	"github.com/gorilla/websocket"
)

const defaultMinBytes = 10 * 1024

type config struct {
	host      string
	user      string
	password  string
	camera    string
	profile   int
	timeout   time.Duration
	attempts  int
	minBytes  int64
	verifySSL bool
	debugFile string
}

type apiResponse struct {
	Success bool `json:"success"`
	Error   any  `json:"error"`
	Data    struct {
		SID       string `json:"sid"`
		SynoToken string `json:"synotoken"`
		Cameras   []struct {
			ID   int    `json:"id"`
			Name string `json:"name"`
		} `json:"cameras"`
	} `json:"data"`
}

func main() {
	log.SetFlags(log.Ldate | log.Ltime)
	loadDotEnv(".env")
	debugFile := flag.String("debug-screenshot", os.Getenv("WATCHDOG_DEBUG_SCREENSHOT"), "save the validated screenshot as a PNG")
	flag.Parse()
	cfg, err := loadConfig()
	if err != nil {
		log.Fatal(err)
	}
	cfg.debugFile = *debugFile

	check := func() error {
		return checkWebSocketStream(cfg)
	}
	if err := runWatchdog(cfg.attempts, check); err != nil {
		log.Fatalf("watchdog failed: %v", err)
	}
}

func runWatchdog(attempts int, check func() error) error {
	var lastCheckErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		err := check()
		if err == nil {
			log.Printf("WebSocket screenshot check succeeded (attempt %d/%d)", attempt, attempts)
			return nil
		}
		lastCheckErr = err
		log.Printf("WebSocket screenshot check failed (attempt %d/%d): %v", attempt, attempts, err)
	}
	return fmt.Errorf("screenshot check failed after %d attempts: %w", attempts, lastCheckErr)
}

func checkWebSocketStream(cfg config) error {
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: !cfg.verifySSL} //nolint:gosec -- controlled by SYNO_VERIFY_SSL
	httpClient := &http.Client{Transport: &http.Transport{TLSClientConfig: tlsConfig}}

	loginCtx, cancelLogin := context.WithTimeout(context.Background(), 15*time.Second)
	sid, token, err := login(loginCtx, httpClient, cfg)
	cancelLogin()
	if err != nil {
		return err
	}
	defer logout(cfg, httpClient, sid)

	listCtx, cancelList := context.WithTimeout(context.Background(), 15*time.Second)
	cameraID, err := findCamera(listCtx, httpClient, cfg, sid)
	cancelList()
	if err != nil {
		return err
	}

	ctx, cancelStream := context.WithTimeout(context.Background(), cfg.timeout)
	defer cancelStream()

	params := url.Values{
		"method": {"MixStream"}, "blMux": {"true"}, "browser": {"2"},
		"stmSrc": {"0"}, "blLiveSharing": {"true"}, "blAudio": {"false"},
		"profile": {strconv.Itoa(cfg.profile)}, "pause": {"false"}, "dsId": {"0"},
		"SynoToken": {token}, "id": {strconv.Itoa(cameraID)},
	}
	wsURL := "wss://" + cfg.host + "/ss_webstream_task/?" + params.Encode()
	headers := http.Header{
		"Origin":          {"https://" + cfg.host},
		"User-Agent":      {"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/150.0.0.0 Safari/537.36"},
		"Accept-Language": {"en-US,en;q=0.9"},
		"Cache-Control":   {"no-cache"},
		"Pragma":          {"no-cache"},
		"Cookie":          {"id=" + sid},
	}
	dialer := websocket.Dialer{HandshakeTimeout: 15 * time.Second, TLSClientConfig: tlsConfig}
	conn, response, err := dialer.DialContext(ctx, wsURL, headers)
	if err != nil {
		if response != nil {
			return fmt.Errorf("open WebSocket: HTTP %s: %w", response.Status, err)
		}
		return fmt.Errorf("open WebSocket: %w", err)
	}
	defer conn.Close()
	conn.SetReadLimit(32 << 20)
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetReadDeadline(deadline)
	}

	decoder := frameDecoder{minBytes: cfg.minBytes, debugFile: cfg.debugFile}
	for {
		messageType, message, err := conn.ReadMessage()
		if err != nil {
			if ctx.Err() != nil {
				return fmt.Errorf("timed out after %s", cfg.timeout)
			}
			return fmt.Errorf("read WebSocket: %w", err)
		}
		if messageType != websocket.BinaryMessage {
			continue
		}
		header, payload := unwrapFrame(message)
		if !pythonAcceptsPayload(header, payload) {
			continue
		}
		done, err := decoder.feed(payload)
		if err != nil {
			// app.py ignores parser/decoder errors and keeps waiting for a
			// decodable keyframe until the attempt timeout expires.
			continue
		}
		if done {
			return nil
		}
	}
}

type frameDecoder struct {
	decoder   hevc.Decoder
	minBytes  int64
	debugFile string
	backend   transportBackend
	seenVPS   bool
	seenSPS   bool
	seenPPS   bool
}

type transportBackend uint8

const (
	transportUnknown transportBackend = iota
	transportFMP4
	transportAnnexB
)

func (d *frameDecoder) feed(payload []byte) (bool, error) {
	if d.backend == transportUnknown {
		switch {
		case len(payload) >= 4 && isMP4Box(payload[:4]):
			d.backend = transportFMP4
		case startsAnnexB(payload):
			d.backend = transportAnnexB
		default:
			return false, nil
		}
	}

	body := payload
	var nals []hevc.NALUnit
	if d.backend == transportFMP4 {
		body = h265Body(payload)
		if len(body) == 0 {
			return false, nil
		}
		nals = splitPythonFMP4Body(body)
	} else {
		nals = hevc.SplitAnnexB(body)
	}
	if len(body) == 0 {
		return false, nil
	}

	for _, nal := range nals {
		switch nal.Type {
		case hevc.NALVPS:
			d.seenVPS = true
			d.seenSPS = false
			d.seenPPS = false
		case hevc.NALSPS:
			if !d.seenVPS {
				continue
			}
			d.seenSPS = true
		case hevc.NALPPS:
			if !d.seenSPS {
				continue
			}
			d.seenPPS = true
		default:
			if !d.seenPPS {
				continue
			}
		}
		pictures, err := d.decoder.DecodeNAL(nal)
		if err != nil {
			return false, fmt.Errorf("NAL type %d from %d-byte payload: %w", nal.Type, len(body), err)
		}
		for _, picture := range pictures {
			if !nal.Type.IsIRAP() {
				picture.Release()
				continue
			}
			valid, err := d.handlePicture(picture)
			picture.Release()
			if err != nil || valid {
				return valid, err
			}
		}
	}
	return false, nil
}

func splitPythonFMP4Body(body []byte) []hevc.NALUnit {
	if nals, ok := splitCompleteHVCC(body); ok {
		return nals
	}
	return hevc.SplitAnnexB(body)
}

func splitCompleteHVCC(data []byte) ([]hevc.NALUnit, bool) {
	var nals []hevc.NALUnit
	for offset := 0; offset+4 <= len(data); {
		length := int(binary.BigEndian.Uint32(data[offset : offset+4]))
		offset += 4
		if length == 0 || length > 5_000_000 || offset+length > len(data) {
			return nil, false
		}
		nal, ok := hevc.ParseNAL(data[offset : offset+length])
		if !ok {
			return nil, false
		}
		nals = append(nals, nal)
		offset += length
		if offset == len(data) {
			return nals, true
		}
	}
	return nil, false
}

func startsAnnexB(data []byte) bool {
	return len(data) >= 3 && data[0] == 0 && data[1] == 0 &&
		(data[2] == 1 || (len(data) >= 4 && data[2] == 0 && data[3] == 1))
}

func (d *frameDecoder) handlePicture(picture *hevc.Picture) (bool, error) {
	img, err := pictureImage(picture)
	if err != nil {
		return false, err
	}
	var output bytes.Buffer
	if err := png.Encode(&output, img); err != nil {
		return false, fmt.Errorf("encode PNG: %w", err)
	}
	if int64(output.Len()) <= d.minBytes {
		return false, fmt.Errorf("decoded screenshot is too small: %d bytes (must be greater than %d)", output.Len(), d.minBytes)
	}
	if d.debugFile != "" {
		if err := os.WriteFile(d.debugFile, output.Bytes(), 0o644); err != nil {
			return false, fmt.Errorf("save debug screenshot %q: %w", d.debugFile, err)
		}
		log.Printf("saved debug screenshot %s (%d bytes)", d.debugFile, output.Len())
	}
	return true, nil
}

func pictureImage(picture *hevc.Picture) (image.Image, error) {
	if picture.BitDepth > 8 || picture.BitDepthC > 8 {
		return nil, fmt.Errorf("unsupported %d/%d-bit HEVC picture", picture.BitDepth, picture.BitDepthC)
	}
	if picture.StrideY <= 0 || len(picture.Y) < picture.StrideY {
		return nil, fmt.Errorf("invalid HEVC luma plane: stride=%d length=%d", picture.StrideY, len(picture.Y))
	}
	planeHeight := len(picture.Y) / picture.StrideY
	width := min(picture.Width, picture.StrideY)
	height := min(picture.Height, planeHeight)
	full := image.Rect(0, 0, width, height)
	crop := image.Rect(picture.CropX, picture.CropY, picture.CropX+picture.CropW, picture.CropY+picture.CropH).Intersect(full)
	if crop.Empty() {
		crop = full
	}
	// Picture.ChromaFormat uses HEVC chroma_format_idc values: 0=mono,
	// 1=4:2:0, 2=4:2:2, 3=4:4:4.
	if picture.ChromaFormat == 0 {
		img := &image.Gray{Pix: picture.Y, Stride: picture.StrideY, Rect: full}
		return img.SubImage(crop), nil
	}
	ratio := image.YCbCrSubsampleRatio420
	chromaHeight := (height + 1) / 2
	switch picture.ChromaFormat {
	case 1:
	case 2:
		ratio = image.YCbCrSubsampleRatio422
		chromaHeight = height
	case 3:
		ratio = image.YCbCrSubsampleRatio444
		chromaHeight = height
	default:
		return nil, fmt.Errorf("unsupported HEVC chroma format %d", picture.ChromaFormat)
	}
	if picture.StrideC <= 0 || len(picture.Cb) < picture.StrideC*chromaHeight || len(picture.Cr) < picture.StrideC*chromaHeight {
		return nil, fmt.Errorf("invalid HEVC chroma planes: stride=%d height=%d lengths=%d/%d", picture.StrideC, chromaHeight, len(picture.Cb), len(picture.Cr))
	}
	img := &image.YCbCr{
		Y: picture.Y, Cb: picture.Cb, Cr: picture.Cr,
		YStride: picture.StrideY, CStride: picture.StrideC,
		SubsampleRatio: ratio, Rect: full,
	}
	return img.SubImage(crop), nil
}

func h265Body(payload []byte) []byte {
	if len(payload) < 4 {
		return nil
	}
	switch string(payload[:4]) {
	case "mdat":
		return payload[4:]
	case "moof":
		end := moofEnd(payload)
		if end == 0 || end >= len(payload) {
			return nil
		}
		tail := payload[end:]
		if len(tail) >= 8 && string(tail[4:8]) == "mdat" {
			size := int(binary.BigEndian.Uint32(tail[:4]))
			if size >= 8 && size <= len(tail) {
				return tail[8:size]
			}
			return tail[8:]
		}
		return tail
	case "ftyp", "styp", "moov", "sidx", "free", "skip":
		return nil
	default:
		return payload
	}
}

func moofEnd(payload []byte) int {
	allowed := map[string]bool{"mfhd": true, "traf": true, "tref": true, "mvex": true, "udta": true}
	offset, lastEnd := 4, 0
	for offset+8 <= len(payload) {
		size := int(binary.BigEndian.Uint32(payload[offset : offset+4]))
		name := string(payload[offset+4 : offset+8])
		if !allowed[name] || size < 8 || offset+size > len(payload) {
			break
		}
		offset += size
		lastEnd = offset
	}
	return lastEnd
}

func pythonAcceptsPayload(header map[string]string, payload []byte) bool {
	if len(payload) == 0 {
		return false
	}
	mediaType := header["mediaType"]
	if mediaType == "" {
		mediaType = header["mediatype"]
	}
	if mediaType != "" && mediaType != "1" {
		return false
	}
	return !looksLikeHEVCNALWithoutStartCode(payload)
}

func looksLikeHEVCNALWithoutStartCode(data []byte) bool {
	if len(data) < 2 {
		return false
	}
	switch (data[0] >> 1) & 0x3f {
	case 32, 33, 34, 39, 40:
		return true
	default:
		return false
	}
}

func isMP4Box(prefix []byte) bool {
	if len(prefix) < 4 {
		return false
	}
	switch string(prefix[:4]) {
	case "ftyp", "styp", "moov", "moof", "mdat", "sidx", "free", "skip":
		return true
	default:
		return false
	}
}

func unwrapFrame(data []byte) (map[string]string, []byte) {
	if len(data) < 4 {
		return nil, data
	}
	headerLength := int(binary.BigEndian.Uint32(data[:4]))
	if headerLength == 0 || headerLength > 512 || 4+headerLength > len(data) {
		return nil, data
	}
	rawHeader := string(data[4 : 4+headerLength])
	if !strings.Contains(rawHeader, "=") {
		return nil, data
	}
	header := make(map[string]string)
	for _, part := range strings.Split(rawHeader, "&") {
		key, value, ok := strings.Cut(part, "=")
		if ok {
			header[key] = value
		}
	}
	return header, data[4+headerLength:]
}

func login(ctx context.Context, client *http.Client, cfg config) (string, string, error) {
	params := url.Values{
		"api": {"SYNO.API.Auth"}, "version": {"6"}, "method": {"login"},
		"account": {cfg.user}, "passwd": {cfg.password},
		"session": {"SurveillanceStation"}, "format": {"sid"},
	}
	var response apiResponse
	if err := getJSON(ctx, client, endpoint(cfg, params), &response); err != nil {
		return "", "", fmt.Errorf("login: %w", err)
	}
	if !response.Success || response.Data.SID == "" {
		return "", "", fmt.Errorf("login rejected: %v", response.Error)
	}
	return response.Data.SID, response.Data.SynoToken, nil
}

func findCamera(ctx context.Context, client *http.Client, cfg config, sid string) (int, error) {
	params := url.Values{
		"api": {"SYNO.SurveillanceStation.Camera"}, "version": {"8"},
		"method": {"List"}, "basic": {"true"}, "streamInfo": {"true"}, "_sid": {sid},
	}
	var response apiResponse
	if err := getJSON(ctx, client, endpoint(cfg, params), &response); err != nil {
		return 0, fmt.Errorf("list cameras: %w", err)
	}
	if !response.Success {
		return 0, fmt.Errorf("list cameras rejected: %v", response.Error)
	}
	for _, camera := range response.Data.Cameras {
		if strings.EqualFold(strings.TrimSpace(camera.Name), cfg.camera) {
			return camera.ID, nil
		}
	}
	target := strings.ToLower(strings.TrimSpace(cfg.camera))
	for _, camera := range response.Data.Cameras {
		if strings.Contains(strings.ToLower(strings.TrimSpace(camera.Name)), target) {
			return camera.ID, nil
		}
	}
	return 0, fmt.Errorf("camera %q not found", cfg.camera)
}

func logout(cfg config, client *http.Client, sid string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	params := url.Values{
		"api": {"SYNO.API.Auth"}, "version": {"6"}, "method": {"logout"},
		"session": {"SurveillanceStation"}, "_sid": {sid},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint(cfg, params), nil)
	if err != nil {
		return
	}
	resp, err := client.Do(req)
	if err == nil {
		resp.Body.Close()
	}
}

func getJSON(ctx context.Context, client *http.Client, target string, dst any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %s", resp.Status)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(dst); err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}
	return nil
}

func endpoint(cfg config, params url.Values) string {
	return "https://" + cfg.host + "/webapi/entry.cgi?" + params.Encode()
}

func loadConfig() (config, error) {
	timeout, err := positiveInt("SYNO_TIMEOUT", 10)
	if err != nil {
		return config{}, err
	}
	attempts, err := positiveInt("WATCHDOG_ATTEMPTS", 1)
	if err != nil {
		return config{}, err
	}
	minBytes, err := positiveInt("WATCHDOG_MIN_BYTES", defaultMinBytes)
	if err != nil {
		return config{}, err
	}
	profile, err := nonNegativeInt("SYNO_PROFILE", 0)
	if err != nil {
		return config{}, err
	}
	cfg := config{
		host: strings.TrimSpace(os.Getenv("SYNO_HOST")), user: os.Getenv("SYNO_USER"),
		password: os.Getenv("SYNO_PASSWORD"), camera: strings.TrimSpace(os.Getenv("SYNO_CAMERA")),
		profile: profile, timeout: time.Duration(timeout) * time.Second,
		attempts: attempts, minBytes: int64(minBytes), verifySSL: envBool("SYNO_VERIFY_SSL", true),
	}
	var missing []string
	for name, value := range map[string]string{
		"SYNO_HOST": cfg.host, "SYNO_USER": cfg.user, "SYNO_PASSWORD": cfg.password, "SYNO_CAMERA": cfg.camera,
	} {
		if value == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return config{}, fmt.Errorf("missing required configuration: %s", strings.Join(missing, ", "))
	}
	return cfg, nil
}

func positiveInt(name string, fallback int) (int, error) {
	value, err := strconv.Atoi(envDefault(name, strconv.Itoa(fallback)))
	if err != nil || value < 1 {
		return 0, fmt.Errorf("%s must be a positive integer", name)
	}
	return value, nil
}

func nonNegativeInt(name string, fallback int) (int, error) {
	value, err := strconv.Atoi(envDefault(name, strconv.Itoa(fallback)))
	if err != nil || value < 0 {
		return 0, fmt.Errorf("%s must be a non-negative integer", name)
	}
	return value, nil
}

func envBool(name string, fallback bool) bool {
	raw, ok := os.LookupEnv(name)
	if !ok {
		return fallback
	}
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "0", "false", "no", "off":
		return false
	default:
		return true
	}
}

func envDefault(name, fallback string) string {
	if value, ok := os.LookupEnv(name); ok && value != "" {
		return value
	}
	return fallback
}

func loadDotEnv(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		name = strings.TrimSpace(strings.TrimPrefix(name, "export "))
		value = strings.Trim(strings.TrimSpace(value), "\"'")
		if name != "" {
			if _, exists := os.LookupEnv(name); !exists {
				_ = os.Setenv(name, value)
			}
		}
	}
}
