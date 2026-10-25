# Hosting

Rustle usually runs on your computer, started by your agent. Some agents can't start a program on your computer, claude.ai among them. For those, Rustle can run on a server and offer the same tools over HTTP, at a private address.

## On Vercel, in one command

```sh
rustle-mcp deploy
```

It logs you in to Vercel if you aren't already (through the Vercel CLI, which Node's `npx` fetches), puts Rustle on Vercel's free plan, checks that YouTube answers it, and prints the server's address.

To connect claude.ai, open Settings, then Connectors, add a custom connector, paste the address and choose No sign-in. For Claude Code, Codex and Cursor, the command prints a line to run.

The server runs the version of Rustle that deployed it. After you update Rustle, run the command again; the address stays the same. `--name` sets the Vercel project's name (`rustle-mcp` by default), and `--region` the region it runs in (`fra1`, Frankfurt, by default).

## Why it goes through Cloudflare

YouTube refuses most data-center addresses with "Sign in to confirm you're not a bot". So when the server starts, it registers a device with Cloudflare WARP, a free VPN, and sends its YouTube requests through that tunnel. They arrive from an address YouTube trusts far more than the host's.

How far YouTube trusts WARP depends on the region and changes over time: in October 2026 it accepted WARP's exits in six of the ten regions we tried. So `deploy` sets up two servers: one in your region, and a backup in Washington, or in Frankfurt if yours is Washington. When YouTube refuses one, it hands the call to the other and returns that answer.

## On other hosts

Any host that runs containers, such as Railway or Render, can build the repository's `Dockerfile`. Set `RUSTLE_TOKEN` to a long random string; the address is then `https://your-host/mcp/<token>`. Opening that address in a browser shows how to connect each client.

`RUSTLE_BACKUP` can name a second server's address, to take the calls YouTube refuses. `HTTPS_PROXY` can name a proxy, such as a paid residential one, which Rustle then uses only while YouTube refuses the tunnel. [Configuration](configuration.md) lists every setting.

## What changes on a server

Local files are off unless `RUSTLE_MEDIA_ROOTS` names folders on the server.

Vercel caps a response at 4.5 MB. A minute of sound is often larger, so `listen` can fail there for clients that receive audio. Claude gets a picture of the sound instead, which fits.

A hosted Rustle contacts what a local one does: the videos your agent asks for and YouTube's visitor id. It also contacts Cloudflare, where it registers a free WARP device each time it starts. Like a local one, it has no telemetry and calls no AI service: what it decodes goes only to the agent that asked, whose own model does the watching.
