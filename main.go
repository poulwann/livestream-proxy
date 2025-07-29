package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

type StreamProxy struct {
	mu            sync.RWMutex
	clients       map[chan []byte]bool
	headerBuffer  []byte
	segmentBuffer [][]byte
	maxSegments   int
	isRunning     bool
	ctx           context.Context
	cancel        context.CancelFunc
}

func NewStreamProxy(maxSegments int) *StreamProxy {
	ctx, cancel := context.WithCancel(context.Background())
	return &StreamProxy{
		clients:       make(map[chan []byte]bool),
		segmentBuffer: make([][]byte, 0, maxSegments),
		maxSegments:   maxSegments,
		ctx:           ctx,
		cancel:        cancel,
	}
}

func (sp *StreamProxy) AddClient() chan []byte {
	sp.mu.Lock()
	defer sp.mu.Unlock()

	client := make(chan []byte, 100)
	sp.clients[client] = true

	if sp.headerBuffer != nil {
		select {
		case client <- sp.headerBuffer:
		default:
			log.Println("Failed to send header to new client")
		}
	}

	for _, segment := range sp.segmentBuffer {
		select {
		case client <- segment:
		default:
			log.Println("Failed to send buffered segment to new client")
			break
		}
	}

	log.Printf("Added new client, total clients: %d", len(sp.clients))
	return client
}

func (sp *StreamProxy) RemoveClient(client chan []byte) {
	sp.mu.Lock()
	defer sp.mu.Unlock()

	if _, exists := sp.clients[client]; exists {
		delete(sp.clients, client)
		close(client)
		log.Printf("Removed client, total clients: %d", len(sp.clients))
	}
}

func (sp *StreamProxy) broadcast(data []byte, isHeader bool) {
	sp.mu.Lock()
	defer sp.mu.Unlock()

	if isHeader {
		sp.headerBuffer = make([]byte, len(data))
		copy(sp.headerBuffer, data)
		log.Printf("Stored header buffer of %d bytes", len(data))
	} else {
		segment := make([]byte, len(data))
		copy(segment, data)
		sp.segmentBuffer = append(sp.segmentBuffer, segment)

		if len(sp.segmentBuffer) > sp.maxSegments {
			sp.segmentBuffer = sp.segmentBuffer[1:]
		}
	}

	deadClients := make([]chan []byte, 0)
	for client := range sp.clients {
		select {
		case client <- data:
		default:
			deadClients = append(deadClients, client)
		}
	}

	for _, client := range deadClients {
		delete(sp.clients, client)
		close(client)
	}

	if len(deadClients) > 0 {
		log.Printf("Removed %d dead clients", len(deadClients))
	}
}

func (sp *StreamProxy) StartStreaming(streamURL, cookieFile string) error {
	sp.mu.Lock()
	if sp.isRunning {
		sp.mu.Unlock()
		return fmt.Errorf("streaming already running")
	}
	sp.isRunning = true
	sp.mu.Unlock()
	fmt.Println(streamURL)

	cmd := exec.CommandContext(sp.ctx, "yt-dlp",
		"--cookies", cookieFile,
		"--get-url",
		streamURL)
	fmt.Println(cmd)
	output, err := cmd.Output()

	if err != nil {
		log.Printf("Failed to get direct URL, trying DASH method: %v", err)
		go sp.streamFromURL(streamURL, cookieFile, true) // Force DASH mode
		return nil
	}

	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	if len(lines) == 0 {
		log.Printf("No URL returned, trying DASH method")
		go sp.streamFromURL(streamURL, cookieFile, true) // Force DASH mode
		return nil
	}

	actualURL := lines[0]
	log.Printf("Got actual stream URL: %s", actualURL)
	log.Printf("============================")

	isDash := strings.Contains(actualURL, "manifest/dash") ||
		strings.Contains(actualURL, ".mpd") ||
		strings.Contains(actualURL, "/dash/")

	isHLS := strings.Contains(actualURL, ".m3u8") ||
		strings.Contains(actualURL, "/hls/") ||
		strings.Contains(actualURL, "playlist.m3u8")

	isDirectVideo := strings.Contains(actualURL, ".mp4") ||
		strings.Contains(actualURL, ".mkv") ||
		strings.Contains(actualURL, ".avi") ||
		strings.Contains(actualURL, ".webm") ||
		strings.Contains(actualURL, ".flv")

	if isHLS || isDirectVideo {
		isDash = false
		if isHLS {
			log.Printf("Detected HLS stream, using regular FFmpeg method")
		} else {
			log.Printf("Detected direct video file, using regular FFmpeg method")
		}
	}

	log.Printf("Stream type detection - DASH: %t, HLS: %t, Direct: %t", isDash, isHLS, isDirectVideo)

	go sp.streamFromURL(actualURL, cookieFile, isDash)

	return nil
}

func (sp *StreamProxy) streamFromURL(url, cookieFile string, isDash bool) {
	defer func() {
		sp.mu.Lock()
		sp.isRunning = false
		sp.mu.Unlock()
		log.Println("Stream processing stopped")
	}()

	log.Printf("Starting stream processing from URL: %s (DASH: %t)", url, isDash)

	if _, err := exec.LookPath("ffmpeg"); err != nil {
		log.Printf("FFmpeg not found in PATH: %v", err)
		return
	}

	maxRetries := 5
	retryDelay := 2 * time.Second

	for attempt := 1; attempt <= maxRetries; attempt++ {
		select {
		case <-sp.ctx.Done():
			log.Println("Context cancelled, stopping stream")
			return
		default:
		}

		log.Printf("Stream attempt %d/%d", attempt, maxRetries)

		var success bool
		if isDash {
			success = sp.runFFmpegStreamDash(url, cookieFile, attempt)
		} else {
			success = sp.runFFmpegStream(url, attempt)

			if !success {
				if strings.Contains(url, "manifest/dash") || strings.Contains(url, ".mpd") || strings.Contains(url, "/dash/") {
					log.Printf("Regular method failed for potential DASH URL, trying DASH method as fallback")
					success = sp.runFFmpegStreamDash(url, cookieFile, attempt)
				}
			}
		}

		if success {
			log.Printf("Stream ended, attempting restart in %v", retryDelay)
			time.Sleep(retryDelay)
			retryDelay = time.Duration(float64(retryDelay) * 1.5)
			continue
		} else {
			break
		}
	}

	log.Printf("Stream failed after %d attempts", maxRetries)
}

func (sp *StreamProxy) runFFmpegStream(url string, attempt int) bool {
	args := []string{
		"-re",                  // Read input at its native frame rate
		"-loglevel", "warning", // Reduce log verbosity
		"-i", url,
		"-c", "copy", // Copy streams without re-encoding
		"-f", "mpegts",
		"-avoid_negative_ts", "make_zero",
		"-fflags", "+genpts+flush_packets",
		"-reconnect", "1", // Enable reconnection
		"-reconnect_at_eof", "1", // Reconnect at EOF
		"-reconnect_streamed", "1", // Reconnect for streamed content
		"-reconnect_delay_max", "5", // Max delay between reconnection attempts
		"-timeout", "10000000", // 10 second timeout (in microseconds)
		"-multiple_requests", "1", // Allow multiple HTTP requests
		"-seekable", "0", // Indicate non-seekable stream
		"-flush_packets", "1", // Flush packets immediately
		"-max_delay", "500000", // Max demux delay (500ms)
		"-",
	}

	log.Printf("Running ffmpeg attempt %d with args: %v", attempt, args)

	cmd := exec.CommandContext(sp.ctx, "ffmpeg", args...)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		log.Printf("Failed to create stdout pipe: %v", err)
		return false
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		log.Printf("Failed to create stderr pipe: %v", err)
		return false
	}

	go func() {
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			line := scanner.Text()
			if line != "" {
				log.Printf("FFmpeg[%d]: %s", attempt, line)
			}
		}
	}()

	log.Printf("Starting FFmpeg process (attempt %d)...", attempt)
	if err := cmd.Start(); err != nil {
		log.Printf("Failed to start ffmpeg: %v", err)
		return false
	}

	log.Printf("FFmpeg started with PID: %d (attempt %d)", cmd.Process.Pid, attempt)

	reader := bufio.NewReaderSize(stdout, 128*1024)
	buffer := make([]byte, 64*1024)
	headerSent := false
	totalBytes := 0
	lastLogTime := time.Now()
	lastDataTime := time.Now()
	noDataTimeout := 30 * time.Second

	log.Printf("Starting to read from FFmpeg stdout (attempt %d)...", attempt)

	for {
		select {
		case <-sp.ctx.Done():
			log.Println("Context cancelled, stopping stream")
			if cmd.Process != nil {
				cmd.Process.Kill()
			}
			return false
		default:
		}

		n, err := reader.Read(buffer)

		if err != nil {
			if err == io.EOF {
				log.Printf("Stream ended (EOF) on attempt %d", attempt)
				break
			} else {
				if time.Since(lastDataTime) > noDataTimeout {
					log.Printf("No data timeout reached on attempt %d", attempt)
					break
				}
				log.Printf("Error reading stream on attempt %d: %v", attempt, err)
				time.Sleep(100 * time.Millisecond)
				continue
			}
		}

		if n > 0 {
			lastDataTime = time.Now()
			totalBytes += n
			data := make([]byte, n)
			copy(data, buffer[:n])

			if !headerSent {
				log.Printf("Got first data chunk of %d bytes on attempt %d - sending as header", n, attempt)
				sp.broadcast(data, true)
				headerSent = true
			} else {
				sp.broadcast(data, false)
			}

			now := time.Now()
			if now.Sub(lastLogTime) > 10*time.Second {
				sp.mu.RLock()
				clientCount := len(sp.clients)
				sp.mu.RUnlock()
				log.Printf("Attempt %d: Read %d bytes (total: %d), %d clients connected", attempt, n, totalBytes, clientCount)
				lastLogTime = now
			}
		}
	}

	log.Printf("Stream processing completed on attempt %d, total bytes: %d", attempt, totalBytes)

	if err := cmd.Wait(); err != nil {
		if exitError, ok := err.(*exec.ExitError); ok {
			exitCode := exitError.ExitCode()
			log.Printf("FFmpeg process ended with exit code %d on attempt %d", exitCode, attempt)
			return exitCode <= 1
		}
		log.Printf("FFmpeg process ended with error on attempt %d: %v", attempt, err)
		return true
	}

	log.Printf("FFmpeg process ended normally on attempt %d", attempt)
	return true
}

func (sp *StreamProxy) runFFmpegStreamDash(url, cookieFile string, attempt int) bool {

	ytdlpArgs := []string{
		"--cookies", cookieFile,
		"--format", "best[height<=720]/best",
		"--output", "-",
		url,
	}

	ffmpegArgs := []string{
		"-re",
		"-loglevel", "warning",
		"-i", "pipe:0",
		"-c", "copy",
		"-f", "mpegts",
		"-avoid_negative_ts", "make_zero",
		"-fflags", "+genpts+flush_packets",
		"-flush_packets", "1",
		"-max_delay", "500000",
		"-",
	}

	log.Printf("Running DASH pipeline attempt %d: yt-dlp -> ffmpeg", attempt)
	log.Printf("yt-dlp args: %v", ytdlpArgs)
	log.Printf("ffmpeg args: %v", ffmpegArgs)

	ytdlpCmd := exec.CommandContext(sp.ctx, "yt-dlp", ytdlpArgs...)
	ffmpegCmd := exec.CommandContext(sp.ctx, "ffmpeg", ffmpegArgs...)

	ytdlpStdout, err := ytdlpCmd.StdoutPipe()
	if err != nil {
		log.Printf("Failed to create yt-dlp stdout pipe: %v", err)
		return false
	}

	ffmpegCmd.Stdin = ytdlpStdout

	ffmpegStdout, err := ffmpegCmd.StdoutPipe()
	if err != nil {
		log.Printf("Failed to create ffmpeg stdout pipe: %v", err)
		return false
	}

	ffmpegStderr, err := ffmpegCmd.StderrPipe()
	if err != nil {
		log.Printf("Failed to create ffmpeg stderr pipe: %v", err)
		return false
	}

	go func() {
		scanner := bufio.NewScanner(ffmpegStderr)
		for scanner.Scan() {
			line := scanner.Text()
			if line != "" {
				log.Printf("FFmpeg-DASH[%d]: %s", attempt, line)
			}
		}
	}()

	if err := ytdlpCmd.Start(); err != nil {
		log.Printf("Failed to start yt-dlp: %v", err)
		return false
	}

	if err := ffmpegCmd.Start(); err != nil {
		log.Printf("Failed to start ffmpeg: %v", err)
		ytdlpCmd.Process.Kill()
		return false
	}

	log.Printf("Started DASH pipeline: yt-dlp (PID: %d) -> ffmpeg (PID: %d) (attempt %d)",
		ytdlpCmd.Process.Pid, ffmpegCmd.Process.Pid, attempt)

	reader := bufio.NewReaderSize(ffmpegStdout, 128*1024)
	buffer := make([]byte, 64*1024)
	headerSent := false
	totalBytes := 0
	lastLogTime := time.Now()
	lastDataTime := time.Now()
	noDataTimeout := 30 * time.Second

	log.Printf("Starting to read from FFmpeg stdout (DASH attempt %d)...", attempt)

	for {
		select {
		case <-sp.ctx.Done():
			log.Println("Context cancelled, stopping DASH stream")
			if ytdlpCmd.Process != nil {
				ytdlpCmd.Process.Kill()
			}
			if ffmpegCmd.Process != nil {
				ffmpegCmd.Process.Kill()
			}
			return false
		default:
		}

		n, err := reader.Read(buffer)

		if err != nil {
			if err == io.EOF {
				log.Printf("DASH stream ended (EOF) on attempt %d", attempt)
				break
			} else {
				if time.Since(lastDataTime) > noDataTimeout {
					log.Printf("DASH no data timeout reached on attempt %d", attempt)
					break
				}
				log.Printf("Error reading DASH stream on attempt %d: %v", attempt, err)
				time.Sleep(100 * time.Millisecond)
				continue
			}
		}

		if n > 0 {
			lastDataTime = time.Now()
			totalBytes += n
			data := make([]byte, n)
			copy(data, buffer[:n])

			if !headerSent {
				log.Printf("Got first DASH data chunk of %d bytes on attempt %d - sending as header", n, attempt)
				sp.broadcast(data, true)
				headerSent = true
			} else {
				sp.broadcast(data, false)
			}

			now := time.Now()
			if now.Sub(lastLogTime) > 10*time.Second {
				sp.mu.RLock()
				clientCount := len(sp.clients)
				sp.mu.RUnlock()
				log.Printf("DASH attempt %d: Read %d bytes (total: %d), %d clients connected", attempt, n, totalBytes, clientCount)
				lastLogTime = now
			}
		}
	}

	log.Printf("DASH stream processing completed on attempt %d, total bytes: %d", attempt, totalBytes)

	ytdlpCmd.Wait()
	if err := ffmpegCmd.Wait(); err != nil {
		if exitError, ok := err.(*exec.ExitError); ok {
			exitCode := exitError.ExitCode()
			log.Printf("FFmpeg DASH process ended with exit code %d on attempt %d", exitCode, attempt)
			return exitCode <= 1
		}
		log.Printf("FFmpeg DASH process ended with error on attempt %d: %v", attempt, err)
		return true
	}

	log.Printf("DASH pipeline ended normally on attempt %d", attempt)
	return true
}

func (sp *StreamProxy) Stop() {
	sp.cancel()

	sp.mu.Lock()
	defer sp.mu.Unlock()

	for client := range sp.clients {
		close(client)
	}
	sp.clients = make(map[chan []byte]bool)
}

func (sp *StreamProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	log.Printf("New client connected: %s", r.RemoteAddr)

	sp.mu.RLock()
	isRunning := sp.isRunning
	hasHeader := sp.headerBuffer != nil
	segmentCount := len(sp.segmentBuffer)
	sp.mu.RUnlock()

	log.Printf("Stream status - Running: %t, Has Header: %t, Segments: %d", isRunning, hasHeader, segmentCount)

	if !isRunning {
		http.Error(w, "Stream not available - streaming not started", http.StatusServiceUnavailable)
		log.Printf("Client rejected - stream not running")
		return
	}

	w.Header().Set("Content-Type", "video/mp2t")
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Transfer-Encoding", "chunked")

	if hijacker, ok := w.(http.Hijacker); ok {
		conn, bufrw, err := hijacker.Hijack()
		if err == nil {
			defer conn.Close()
			sp.serveStreamDirect(conn, bufrw, r.RemoteAddr)
			return
		}
	}

	sp.serveStreamHTTP(w, r)
}

func (sp *StreamProxy) serveStreamDirect(conn net.Conn, bufrw *bufio.ReadWriter, remoteAddr string) {
	if tcpConn, ok := conn.(*net.TCPConn); ok {
		tcpConn.SetKeepAlive(true)
		tcpConn.SetKeepAlivePeriod(30 * time.Second)
		tcpConn.SetNoDelay(true)

		tcpConn.SetReadBuffer(64 * 1024)
		tcpConn.SetWriteBuffer(256 * 1024)
	}

	response := "HTTP/1.1 200 OK\r\n" +
		"Content-Type: video/mp2t\r\n" +
		"Cache-Control: no-cache, no-store, must-revalidate\r\n" +
		"Connection: keep-alive\r\n" +
		"Transfer-Encoding: chunked\r\n" +
		"\r\n"

	if _, err := bufrw.WriteString(response); err != nil {
		log.Printf("Failed to write headers to %s: %v", remoteAddr, err)
		return
	}
	bufrw.Flush()

	client := sp.AddClient()
	defer sp.RemoveClient(client)

	log.Printf("Direct TCP streaming started for %s", remoteAddr)

	dataReceived := false
	writeTimeout := 10 * time.Second

	for {
		select {
		case data, ok := <-client:
			if !ok {
				log.Printf("Client channel closed: %s", remoteAddr)
				return
			}

			if !dataReceived {
				log.Printf("Sending first data chunk to client %s (%d bytes)", remoteAddr, len(data))
				dataReceived = true
			}

			conn.SetWriteDeadline(time.Now().Add(writeTimeout))

			chunkSize := fmt.Sprintf("%x\r\n", len(data))
			if _, err := bufrw.WriteString(chunkSize); err != nil {
				log.Printf("Error writing chunk size to %s: %v", remoteAddr, err)
				return
			}

			if _, err := bufrw.Write(data); err != nil {
				log.Printf("Error writing data to %s: %v", remoteAddr, err)
				return
			}

			if _, err := bufrw.WriteString("\r\n"); err != nil {
				log.Printf("Error writing chunk trailer to %s: %v", remoteAddr, err)
				return
			}

			if err := bufrw.Flush(); err != nil {
				log.Printf("Error flushing to %s: %v", remoteAddr, err)
				return
			}

		case <-time.After(45 * time.Second):
			if !dataReceived {
				log.Printf("Client %s timed out waiting for data", remoteAddr)
				return
			}
			conn.SetWriteDeadline(time.Now().Add(writeTimeout))
			bufrw.WriteString("0\r\n\r\n")
			bufrw.Flush()
		}
	}
}

func (sp *StreamProxy) serveStreamHTTP(w http.ResponseWriter, r *http.Request) {
	client := sp.AddClient()
	defer sp.RemoveClient(client)

	sp.mu.RLock()
	hasHeader := sp.headerBuffer != nil
	segmentCount := len(sp.segmentBuffer)
	sp.mu.RUnlock()

	if !hasHeader && segmentCount == 0 {
		log.Printf("No data available yet, client will wait for first data")
	}

	dataReceived := false
	writeBuffer := make([]byte, 0, 64*1024)
	lastFlush := time.Now()
	flushInterval := 100 * time.Millisecond

	for {
		select {
		case data, ok := <-client:
			if !ok {
				log.Printf("Client channel closed: %s", r.RemoteAddr)
				return
			}

			if !dataReceived {
				log.Printf("Sending first data chunk to client %s (%d bytes)", r.RemoteAddr, len(data))
				dataReceived = true
			}

			writeBuffer = append(writeBuffer, data...)

			shouldFlush := len(writeBuffer) >= 32*1024 || time.Since(lastFlush) >= flushInterval

			if shouldFlush {
				if _, err := w.Write(writeBuffer); err != nil {
					log.Printf("Error writing to client %s: %v", r.RemoteAddr, err)
					return
				}

				if flusher, ok := w.(http.Flusher); ok {
					flusher.Flush()
				}

				writeBuffer = writeBuffer[:0]
				lastFlush = time.Now()
			}

		case <-r.Context().Done():
			log.Printf("Client disconnected: %s", r.RemoteAddr)
			return

		case <-time.After(30 * time.Second):
			if !dataReceived {
				log.Printf("Client %s timed out waiting for data", r.RemoteAddr)
				http.Error(w, "Stream timeout - no data received", http.StatusRequestTimeout)
				return
			}
		}
	}
}

func main() {
	if len(os.Args) < 3 {
		log.Fatal("Usage: go run main.go <stream_url> <cookie_file> [port]")
	}

	streamURL := os.Args[1]
	cookieFile := os.Args[2]
	port := "8080"

	if len(os.Args) > 3 {
		port = os.Args[3]
	}

	if _, err := os.Stat(cookieFile); os.IsNotExist(err) {
		log.Fatalf("Cookie file does not exist: %s", cookieFile)
	}

	proxy := NewStreamProxy(10)

	defer proxy.Stop()

	log.Println("Starting streaming process...")
	if err := proxy.StartStreaming(streamURL, cookieFile); err != nil {
		log.Fatalf("Failed to start streaming: %v", err)
	}

	log.Println("Waiting for FFmpeg to initialize...")
	time.Sleep(5 * time.Second)

	http.HandleFunc("/stream", proxy.ServeHTTP)
	http.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		proxy.mu.RLock()
		isRunning := proxy.isRunning
		clientCount := len(proxy.clients)
		hasHeader := proxy.headerBuffer != nil
		segmentCount := len(proxy.segmentBuffer)
		proxy.mu.RUnlock()

		fmt.Fprintf(w, "Status: %t\nClients: %d\nHas Header: %t\nSegments: %d\n",
			isRunning, clientCount, hasHeader, segmentCount)
	})
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `
<!DOCTYPE html>
<html>
<head>
    <title>Live Stream Proxy</title>
    <style>
        body { font-family: Arial, sans-serif; margin: 40px; }
        .info { margin: 20px 0; padding: 15px; background: #f5f5f5; border-radius: 5px; }
        .links a { margin-right: 15px; }
    </style>
</head>
<body>
    <h1>Live Stream Proxy</h1>
    
    <div class="info">
    
    <div class="info">
        <h3>External Player Instructions</h3>
        <p><strong>ffplay:</strong></p>
        <code>ffplay -fflags +genpts+discardcorrupt+igndts -reconnect 1 -reconnect_at_eof 1 -reconnect_streamed 1 -reconnect_delay_max 5 -timeout 10000000 -analyzeduration 2000000 -probesize 2000000 -max_delay 500000 -sync audio -framedrop http://SERVER_URL:PORT/stream</code>
    </div>
    Notice the server URL is at the end of this command. Edit it as required.

    <div class="links">
        <a href="/status">Status</a>
    </div>
</body>
</html>
`, port, port, port)
	})
	http.HandleFunc("/test", func(w http.ResponseWriter, r *http.Request) {
		proxy.mu.RLock()
		isRunning := proxy.isRunning
		hasHeader := proxy.headerBuffer != nil
		headerSize := 0
		if proxy.headerBuffer != nil {
			headerSize = len(proxy.headerBuffer)
		}
		segmentCount := len(proxy.segmentBuffer)
		totalSegmentSize := 0
		for _, seg := range proxy.segmentBuffer {
			totalSegmentSize += len(seg)
		}
		clientCount := len(proxy.clients)
		proxy.mu.RUnlock()

		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprintf(w, "Stream Status:\n")
		fmt.Fprintf(w, "Running: %t\n", isRunning)
		fmt.Fprintf(w, "Has Header: %t (size: %d bytes)\n", hasHeader, headerSize)
		fmt.Fprintf(w, "Segments: %d (total size: %d bytes)\n", segmentCount, totalSegmentSize)
		fmt.Fprintf(w, "Active Clients: %d\n", clientCount)
	})

	log.Printf("Starting server on port %s", port)
	log.Printf("Stream URL: http://localhost:%s/stream", port)
	log.Printf("Status: http://localhost:%s/status", port)
	log.Printf("Test: http://localhost:%s/test", port)

	if err := http.ListenAndServe(":"+port, nil); err != nil {
		log.Fatalf("Server failed: %v", err)
	}
}
