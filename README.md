
# Live Stream Proxy

A Go application that proxies live streams using yt-dlp and FFmpeg, allowing multiple clients to connect to a single stream source. Useful for sharing streams to multiple devices from a single source stream. Supports authentication via cookie.

## Features

- Proxies live streams from various sources supported by yt-dlp
- Supports multiple concurrent clients


## Prerequisites

- Go 1.24.5 or later
- FFmpeg - needed for client AND server, you will need ffplay to play the stream as vlc etc wont work (for reasons which is unclear for now).
- yt-dlp
- Cookie file for authentication (if required by the stream source)

## Installation

1. Clone the repository and build the source:
```
git clone https://github.com/poulwann/livestream-proxy
cd livestream-proxy
go build -o livestream-proxy main.go
```
## Usage

Server side:

`./livestream-proxy <stream_url> <cookie_file> [port]`

Keep in mind `ytp-dl` is used in the background. You can use `ytp-dl` to test your stream URL for correctness before running the server. This is recommended.

## Sharing an Authenticated Source

First you need to obtain the cookie from the source you are trying to share.

In Firefox:

1. Install extension: https://addons.mozilla.org/en-US/firefox/addon/cookies-txt/
2. Goto the Stream, log in if not already logged in, then create the cookies.txt file from 'current site' using the add-on.

Now you have cookies.txt which is authenticated to watch the stream. Simply provide this cookie to livestream-proxy.

Example:

`./livestream-proxy <stream_url> cookies.txt 8080`

## Playing the stream

Either use the `play_stream.sh` script with parameters like:

Example:
`/play_stream.sh http://localhost:8080/stream`

Or invoke ffplay manually using:

```
ffplay -fflags +genpts+discardcorrupt+igndts -reconnect 1 -reconnect_at_eof 1 -reconnect_streamed 1 -reconnect_delay_max 5 -timeout 10000000 -analyzeduration 2000000 -probesize 2000000 -max_delay 500000 -sync audio -framedrop http://SERVER_URL:PORT/stream
```
Notice the server is at the end of the command. You will need to install ffmpeg on your operating system from https://ffmpeg.org/download.html
