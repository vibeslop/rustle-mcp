# Rustle

[![CI](https://github.com/vibeslop/rustle-mcp/actions/workflows/ci.yml/badge.svg)](https://github.com/vibeslop/rustle-mcp/actions/workflows/ci.yml)

Rustle is an MCP server that lets AI agents read videos. Give your agent a YouTube link, a web page with a video, a media URL or a local file, and it can read the transcript, look at frames and listen to the sound. The model you already use does the reading; Rustle needs no API key and sends nothing to an inference service.

- **Transcripts** come back as the video's key sentences, verbatim and timestamped, so an hour of speech fits one call. Ask for every word, a span, or only the sentences that mention a topic.
- **Frames** come as labelled contact sheets. Zoom into any region of a frame to read small text or UI, or follow one region frame by frame.
- **Sound** comes as lossless audio, or as a spectrogram with loudness, onsets and silences for clients that can't play audio.
- **Search** YouTube, list a channel, or list the videos on a page or in a folder.

Images are sized for how models are billed and how harnesses resize them, so Claude Code and Codex show the model exactly what Rustle drew.

## Install

Rustle is one file with no dependencies. Frames and sound need [ffmpeg](https://ffmpeg.org) (`brew install ffmpeg`, `sudo apt install ffmpeg`, `winget install ffmpeg`); transcripts and search work without it.

On macOS and Linux, install the latest release and register it with your client in one step:

```sh
curl -fsSL https://raw.githubusercontent.com/vibeslop/rustle-mcp/main/install.sh | sh -s -- claude
```

The binary goes to `~/.local/bin/rustle-mcp`. To register it with a client later, or with another one:

```sh
rustle-mcp setup claude    # Claude Code
rustle-mcp setup codex     # Codex CLI
rustle-mcp setup cursor    # Cursor: prints an install link
rustle-mcp setup           # prints the config for every client
```

`setup` registers the binary by its full path, so the server starts in milliseconds and works from any folder. Restart the client or reconnect its MCP servers afterwards. To update, run the install script again.

You can also download a binary for your platform, Windows included, from [Releases](https://github.com/vibeslop/rustle-mcp/releases), or build one with Go 1.24 or newer: `go install github.com/vibeslop/rustle-mcp@latest`.

For any other client, add this to its MCP server list (`rustle-mcp setup json` prints it with your path filled in):

```json
{
  "mcpServers": {
    "rustle": { "command": "/path/to/rustle-mcp", "args": [] }
  }
}
```

## Tools

Every tool except `find_videos` takes a `source`: a YouTube link or id, a web page, a media or HLS URL, or a local path. There is nothing to open or close, and repeat calls are cached.

| Tool | Use it for | Returns |
| --- | --- | --- |
| `read_transcript` | What is said | The key sentences of the whole video, about a third of the words. `detail: "full"` or a span returns every word; `query` returns matching sentences with context. |
| `watch_video` | What is shown | Contact sheets of frames across the video or a span, each labelled with its time. `fps` samples consecutive frames for motion; `crop` follows one region. |
| `inspect_frame` | Small text, UI, faces | One lossless frame. A `crop` shows a region at full detail, enlarging small ones; `detail: "full"` sends every pixel. |
| `listen_video` | Sound | FLAC audio, or a spectrogram with levels, onsets and silences. |
| `find_videos` | Finding videos | YouTube search results, a channel's videos, or the videos on a page or in a local folder. |

Out-of-range arguments are clamped and unknown ones ignored, each with a note in the result. Errors say what went wrong and how to recover.

## Sources

- **YouTube.** Captions, frames and sound are fetched directly from YouTube, a few seconds at a time, so long videos are never downloaded whole. Live streams aren't supported.
- **Web pages.** Videos in `<video>` tags, OpenGraph and JSON-LD metadata, and YouTube embeds. Players that need JavaScript, DRM or a login aren't supported.
- **Media URLs.** Anything ffmpeg can open, including HLS playlists.
- **Local files.** Any file under your home folder, the temp folder or the client's working directory. A `.srt` or `.vtt` with the same name next to a video is used as its transcript.

## Configuration

All settings are optional environment variables, set in the client's `env` block for the server.

| Variable | Default | Meaning |
| --- | --- | --- |
| `RUSTLE_MEDIA_ROOTS` | home folder | Folders local files may come from, separated by `:` (`;` on Windows). Temp and the working directory are always added. Empty turns local files off. |
| `RUSTLE_CACHE` | `$XDG_CACHE_HOME/rustle-mcp`, else `~/Library/Caches/rustle-mcp` on macOS and `~/.cache/rustle-mcp` elsewhere | Cache folder. |
| `RUSTLE_CACHE_MB` | `2048` | Cache size; the least recently used sources are evicted first. |
| `RUSTLE_TIMEOUT` | `120` | Seconds allowed per ffmpeg operation. |
| `RUSTLE_WORKERS` | CPU count | ffmpeg processes run at once. |
| `RUSTLE_TRANSCRIPT_CHARS` | `45000` | Transcript characters per call. |
| `RUSTLE_FFMPEG`, `RUSTLE_FFPROBE` | found on `PATH` or in the usual install folders | Paths to the ffmpeg binaries. |
| `RUSTLE_WHISPER_MODEL` | unset | A ggml model for whisper.cpp's `whisper-cli`. With both installed, `read_transcript` can transcribe videos that have no captions. |
| `RUSTLE_PREFETCH` | on | At startup, fetch the visitor id YouTube requires, so the first YouTube call is faster. `0` makes Rustle contact YouTube only when asked to. |
| `RUSTLE_WARM` | on | Keep a few ffmpeg processes started and waiting, saving 20–40 ms per decode. `0` turns it off. Not on Windows. |
| `RUSTLE_DEBUG` | unset | `1` adds each call's timing to its result. |
| `RUSTLE_TRACE` | unset | `1` logs every ffmpeg run and network request to stderr. |

## Privacy and limits

Rustle runs on your machine, next to your agent, and has no telemetry. It contacts only the sources your agent asks for, plus YouTube at startup unless `RUSTLE_PREFETCH=0`. It is built for local use: YouTube is far stricter with requests from servers in data centers, so hosting Rustle for others is not supported.

Claude clients can't hear audio, so `listen_video` gives them the spectrogram and measurements instead. CI runs on macOS and Linux; Windows builds are released but untested.

## Development

```sh
git clone https://github.com/vibeslop/rustle-mcp && cd rustle-mcp
go test ./...                  # offline, a few seconds; frame tests need ffmpeg
go run ./scripts/smoke         # builds, then checks every tool against live YouTube
go build -o rustle-mcp . && ./rustle-mcp setup claude
```

Inside a clone, `.mcp.json` offers the local build (`./rustle-mcp`) to Claude Code. [AGENTS.md](AGENTS.md) describes how the code is laid out and what to watch for when changing it, and [CONTRIBUTING.md](CONTRIBUTING.md) how to send a change.

## License

[MIT](LICENSE). Rustle uses only Go's standard library.
