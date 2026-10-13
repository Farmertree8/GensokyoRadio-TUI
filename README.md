# GensokyoRadio-TUI
A Gensokyo Radio TUI client written in Go by Claude with ~15MiB RAM usage

### Setup

1. Install mpv and make sure `mpv` works in a terminal (it must be in PATH).
2. Create a Last.fm API account at https://www.last.fm/api/account/create.
3. Create the file `%APPDATA%\gr-tui\config.json` with your key and secret:

```json
{ "api_key": "YOUR_KEY", "api_secret": "YOUR_SECRET" }
```

4. Build the program:

```
go mod init gr
go mod tidy
go build -ldflags="-s -w" -o gr.exe
```

5. Run `gr.exe -auth` once. It opens the Last.fm approval page and saves a session key to the config file. No password is stored.
6. Run `gr.exe` to listen. Press G to search Google and Q to quit.