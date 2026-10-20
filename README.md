# Rustle

[![CI](https://github.com/vibeslop/rustle-mcp/actions/workflows/ci.yml/badge.svg)](https://github.com/vibeslop/rustle-mcp/actions/workflows/ci.yml)

Rustle lets AI agents watch and listen to videos. Give your agent a YouTube link, a web page, a media URL or a local file, and the model you already use sees the frames and hears the soundtrack, words and all, the way a person watching would.

Most ways of handing a video to a model stop at the transcript. But a transcript says "as you can see here" and shows nothing: not the slide, the code on screen, the chart, the demo, the face, or the music underneath. Rustle gives the model the picture and the sound as well, so it can answer questions about what happened, not only what was said:

- *"Watch `~/Desktop/bug.mov` and tell me where the layout breaks."*
- *"What command does she type at 14:02?"*
- *"Summarize this lecture, including the equations on the board."*
- *"List every screen in this onboarding video and what each one asks for."*
- *"When does the music cut out?"*

Rustle needs no API key and sends nothing to an inference service. It fetches, decodes and sizes the video on your machine; your agent's own model does the watching and listening.

## How it watches and listens

- **Sight.** `watch_video` lays out frames from across a video, or any span of it, as contact sheets with each frame labelled by its time, so the model takes in a whole video at a glance. Frames that barely change share one cell, so a slide that stays up doesn't fill the sheet. To follow motion it samples consecutive frames, up to 8 a second; to follow one part of the screen it crops.
- **A closer look.** `inspect_frame` returns one lossless frame and zooms into any region of it, enlarging small ones up to 4x, so the model can read small text, UI and faces.
- **Hearing what is said.** `read_transcript` returns the video's key sentences, verbatim and timestamped, about a third of the words, so an hour of speech fits in one call. It can also return every word, a span, or only the sentences about a topic, and with whisper.cpp installed it transcribes videos that have no captions.
- **Hearing how it sounds.** `listen_video` returns up to a minute of lossless sound per call: the music, the tone of a voice, a laugh, a pause. `watch_video` can attach short clips to its frames, so the model sees and hears the same moments. Claude can't hear audio, so it gets the sound as a picture instead: a spectrogram, with loudness, stereo balance, onsets and silences measured.
- **What Rustle draws is what the model sees.** Images are planned in the 28 px patches Claude bills by, and kept inside the limits past which Claude Code and Codex shrink and re-encode them. A whole 16:9 frame costs 336 tokens, and zooming into a region costs no more than the frame.
- **Long videos are never downloaded whole.** From YouTube, Rustle fetches only the seconds around each frame or clip it needs, so looking at minute 50 doesn't wait for the 49 before it.

## Install

On macOS and Linux:

```sh
curl -fsSL https://raw.githubusercontent.com/vibeslop/rustle-mcp/main/install.sh | sh
```

On Windows, in PowerShell:

```powershell
irm https://raw.githubusercontent.com/vibeslop/rustle-mcp/main/install.ps1 | iex
```

That's all: it installs one file and asks which of Claude Code, Codex and Cursor to add Rustle to, listing the ones you have. Restart them, then ask your agent about a video. Run the same command again to update.

Frames and sound need [ffmpeg](https://ffmpeg.org) (`brew install ffmpeg`, `sudo apt install ffmpeg`, `winget install ffmpeg`); transcripts and search work without it.

With Go installed, `go run github.com/vibeslop/rustle-mcp@latest setup` does the same. For any other MCP client, `rustle-mcp setup json` prints the config to add.

## Tools

Every tool except `find_videos` takes a `source`: a YouTube link or id, a web page, a media or HLS URL, or a local path. There is nothing to open or close, and repeat calls are cached.

| Tool | Use it for | Returns |
| --- | --- | --- |
| `watch_video` | What is shown | Contact sheets of frames across the video or a span, each labelled with its time: 12 by default, up to 48. `fps` samples consecutive frames for motion; `crop` follows one region; `include_audio` adds short clips of the sound. |
| `inspect_frame` | Small text, UI, faces | One lossless frame. A `crop` shows a region at full detail, enlarging small ones; `detail: "full"` sends every pixel. |
| `read_transcript` | What is said | The key sentences of the whole video, about a third of the words. `detail: "full"` or a span returns every word; `query` returns matching sentences with context. |
| `listen_video` | How it sounds | Up to 60 seconds of FLAC audio, or a spectrogram with levels, onsets and silences. |
| `find_videos` | Finding videos | YouTube search results, a channel's videos, or the videos on a page or in a local folder. |

Out-of-range arguments are clamped and unknown ones ignored, each with a note in the result. Errors say what went wrong and how to recover.

## Sources

- **YouTube.** Captions, frames and sound come straight from YouTube, a few seconds at a time. Live streams aren't supported.
- **Web pages.** Videos in `<video>` tags, OpenGraph and JSON-LD metadata, and YouTube embeds. Players that need JavaScript, DRM or a login aren't supported.
- **Media URLs.** Anything ffmpeg can open, including HLS playlists.
- **Local files.** Any file under your home folder, the temp folder or the client's working directory. A `.srt` or `.vtt` with the same name next to a video is used as its transcript.

## Configuration

All settings are optional environment variables, set in the client's `env` block for the server, or on the host for a hosted server; `rustle-mcp deploy` sets what Vercel needs.

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
| `RUSTLE_TOKEN` | unset | Required by `rustle-mcp http`: the token in the server's address, `/mcp/<token>`, or sent as a bearer token. |
| `HTTPS_PROXY` | unset | A proxy for Rustle's requests. On a hosted server, a backup used while YouTube refuses the WARP tunnel. |
| `RUSTLE_PROXIES` | unset | Proxies to try in order, separated by commas, moving on while YouTube refuses one; `direct` is none. Replaces the hosted server's own list. |
| `WIREGUARD_CONFIG` | unset | In the container: a WireGuard profile, any VPN's, to tunnel through instead of a new WARP device. `RUSTLE_WARP=0` goes without WARP. |
| `RUSTLE_BACKUP` | unset | Another Rustle server's address, which makes the calls YouTube refuses this one. `rustle-mcp deploy` sets it. |
| `RUSTLE_DEBUG` | unset | `1` adds each call's timing to its result. |
| `RUSTLE_TRACE` | unset | `1` logs every ffmpeg run and network request to stderr. |

## Hosting

Rustle can also run on a server, for agents that can't start a program on your computer, such as claude.ai. One command puts it on Vercel's free plan:

```sh
rustle-mcp deploy
```

It logs you in to Vercel first if needed (through the Vercel CLI, which Node's `npx` fetches), deploys the server, checks that YouTube answers it, and prints its private address. In claude.ai, open Settings, then Connectors, add a custom connector, paste the address and choose No sign-in. The command also prints a line for Claude Code, Codex and Cursor. Running it again updates the server and keeps the address.

**How it gets past YouTube.** YouTube refuses most data-center addresses with "Sign in to confirm you're not a bot". So the server registers a free Cloudflare WARP device as it starts and sends its requests through that tunnel, over IPv4, from an address YouTube trusts far more than the host's. Which addresses it trusts varies by region and changes over time: in October 2026 it accepted WARP's exits in Frankfurt, Washington, San Francisco, São Paulo, Sydney and Cape Town, and refused London, Tokyo, Singapore and Mumbai. So `rustle-mcp deploy` sets up two servers, in Frankfurt and in Washington (`--region` picks the first). When YouTube refuses the first, it hands that call to the other and returns its answer.

**Other hosts.** Any host that runs containers can build the repository's `Dockerfile`, such as Railway or Render. Set `RUSTLE_TOKEN` to a long random string; the address is then `https://your-host/mcp/<token>`, and opening the server in a browser shows how to connect each client. `RUSTLE_BACKUP` can name a second server's address, and `HTTPS_PROXY` a proxy, such as a paid residential one, which Rustle then uses only while YouTube refuses the tunnel.

On a server, local paths are off unless `RUSTLE_MEDIA_ROOTS` names folders on it. Vercel caps a response at 4.5 MB, so long `listen_video` clips can fail there for clients that hear audio.

## Privacy and limits

Rustle runs on your machine, next to your agent, and has no telemetry. It contacts only the sources your agent asks for, plus YouTube at startup unless `RUSTLE_PREFETCH=0`. A hosted Rustle does the same from its server, through Cloudflare's WARP, registering a free WARP device each time it starts.

CI runs on macOS, Linux and Windows; on Windows, ffmpeg processes aren't started ahead of calls, so decodes take 20–40 ms longer.

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
