# Configuration

Rustle needs no configuration. Every setting is an optional environment variable. On your computer, put it in the `env` block of Rustle's entry in your agent's MCP config, then restart the agent:

```json
{
  "mcpServers": {
    "rustle": {
      "command": "/path/to/rustle-mcp",
      "env": { "RUSTLE_WHISPER_MODEL": "/path/to/ggml-base.bin" }
    }
  }
}
```

`rustle-mcp setup json` prints this entry with your binary's path. On a hosted server, set the variables on the host instead; `rustle-mcp deploy` sets what Vercel needs (see [Hosting](hosting.md)).

## Words for videos without captions

Rustle reads what a video says from its captions: YouTube's own, or an `.srt` or `.vtt` file with the same name next to a local video. A video without them still shows its frames, but no words, unless Rustle can transcribe the speech on your computer with whisper.cpp, a local speech-to-text program. That takes two things:

1. whisper.cpp's `whisper-cli` on your `PATH`. On macOS, `brew install whisper-cpp`.
2. A ggml model file, such as `ggml-base.bin` from [huggingface.co/ggerganov/whisper.cpp](https://huggingface.co/ggerganov/whisper.cpp), named in **`RUSTLE_WHISPER_MODEL`**.

With both in place, the agent can ask for a transcript when there are no captions, up to ten minutes of speech per call. Nothing leaves your computer.

## Local files

**`RUSTLE_MEDIA_ROOTS`** names the folders local videos may come from, separated by `:` (`;` on Windows). By default it's your home folder. The temp folder and the agent's working folder are always allowed, and an empty value turns local files off.

## Cache and speed

**`RUSTLE_CACHE`** is where Rustle keeps what it fetched: captions, video records and the seconds of YouTube streams it downloaded. By default it's `$XDG_CACHE_HOME/rustle-mcp`, else `~/Library/Caches/rustle-mcp` on macOS and `~/.cache/rustle-mcp` elsewhere.

**`RUSTLE_CACHE_MB`** is the cache's size, 2048 by default. When it fills, the videos used longest ago go first.

**`RUSTLE_WORKERS`** is how many ffmpeg processes run at once. By default it's the number of CPU cores, and at least 2.

**`RUSTLE_TIMEOUT`** is how many seconds one ffmpeg run may take, 120 by default.

**`RUSTLE_WARM`** keeps a few ffmpeg processes started and waiting, which saves 20–40 ms per decode. It's on by default; `0` turns it off. Windows doesn't support it.

**`RUSTLE_PREFETCH`** fetches the visitor id YouTube requires when Rustle starts, so the first YouTube call is faster. It's on by default; `0` makes Rustle contact YouTube only when an agent asks it to.

**`RUSTLE_TRANSCRIPT_CHARS`** is the most transcript one call returns, 45,000 characters by default. Speech past it continues in the next call.

**`RUSTLE_FFMPEG`** and **`RUSTLE_FFPROBE`** are the paths to ffmpeg and ffprobe, for when they're neither on your `PATH` nor in the usual install folders.

## Hosted servers

These matter only when Rustle runs on a server; [Hosting](hosting.md) explains when you'd want that.

**`RUSTLE_TOKEN`** is required by `rustle-mcp http`. It's the secret in the server's address, `/mcp/<token>`, and is also accepted as a bearer token.

**`PORT`** is the port `rustle-mcp http` listens on, 8080 by default. Hosts usually set it.

**`RUSTLE_BACKUP`** is another Rustle server's address. Calls YouTube refuses here go there instead. `rustle-mcp deploy` sets it.

**`HTTPS_PROXY`** is a proxy for Rustle's requests. On a hosted server, Rustle uses it only while YouTube refuses the WARP tunnel.

**`RUSTLE_PROXIES`** lists proxies to try in order, separated by commas; Rustle moves to the next while YouTube refuses one, and `direct` means no proxy. It replaces the hosted server's own list.

**`WIREGUARD_CONFIG`** is a WireGuard profile, from any VPN, for the container to tunnel through instead of registering a new WARP device. **`RUSTLE_WARP=0`** runs the container without WARP.

## Debugging

**`RUSTLE_DEBUG=1`** adds each call's time, in milliseconds, to its result.

**`RUSTLE_TRACE=1`** logs every ffmpeg run and network request to stderr.
