# cc-agent

Lets Claude Code sessions of different people, on different machines and accounts, message each other. A [SoT server](https://github.com/genail/sot-mcp-server) carries the messages.

```
your session ─ cc-agent daemon ─ SoT server ─ cc-agent daemon ─ colleague's session
```

Works on macOS, Linux and WSL 2. Native Windows is not supported.

## Setup

You need an account and a token on a SoT server with federation support. Everyone who takes part does this once:

1. Install the command:

   ```bash
   curl -fsSL https://raw.githubusercontent.com/genail/cc-agent/main/install.sh | sh
   ```

2. Add the server; paste your token when asked:

   ```bash
   cc-agent server add work --url https://sot.example.com
   ```

3. Start the daemon:

   ```bash
   cc-agent service install
   cc-agent status
   ```

   WSL needs systemd for this. Without it, keep `cc-agent daemon` running in a terminal.

4. Add the skills, in Claude Code:

   ```
   /plugin marketplace add genail/cc-agent
   /plugin install cc-agent@cc-agent
   /reload-plugins
   ```

## Use

- "Share this session with bob" makes the current session reachable for bob.
- "Ask bob's api session which version they run" sends a message. The reply arrives in your session.
- "Stop sharing this session" ends it. So does closing the session.

`cc-agent help` lists the commands behind it.

## Security

- **Nothing is shared by default.** A session is reachable only after you register it, only by the people you chose. The server enforces this when listing, sending and delivering. The one exception: whoever you message can reply to the session you wrote from.
- **Messages are marked as external.** Claude Code presents every message between sessions as coming from a session that works for the same user. cc-agent adds a header that says otherwise and what the agent must ask you about, and escapes markup in the text. This instructs the model; it does not technically prevent it from acting. Share sessions only with people you trust, and avoid `bypassPermissions` in them.
- **The server can read messages.** They are not end-to-end encrypted, and stay in its database after delivery.
- **Your token** is in `~/.config/cc-agent/config.json`, readable only by you, and is sent only over HTTPS (or to localhost).

## Limits

- Delivery uses Claude Code's undocumented session sockets, through [claude-code-socket-transport](https://github.com/PeterSR/claude-code-socket-transport). A Claude Code update can break it. Tested with 2.1.332.
- Messages are checked every 5 seconds. A message can arrive twice if the daemon stops at the wrong moment.
- A message has at most 64 KiB; a session holds at most 100 undelivered ones.

## Development

```bash
go test ./...
SOT_SERVER_DIR=../sot-mcp-server go test ./...   # with the end-to-end test
```

Pushing a tag `v*` publishes a release.

## License

[MIT](LICENSE)
