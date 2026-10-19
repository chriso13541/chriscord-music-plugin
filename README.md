# Chriscord Music plugin

Music in voice channels for a Chriscord server, as a **server plugin**. It
plays links from YouTube, SoundCloud, Bandcamp and the other sites yt-dlp
handles, or searches YouTube for whatever you type.

The server runs it. You install it from the admin panel, pick what it may do
and which channels it gets, and the server keeps it running. There's no
token to copy anywhere: the server makes the plugin's identity itself when
you install it.

(The music bot you run on your own machine with a token,
[chriscord-musicbot](https://github.com/chriso13541/chriscord-musicbot), still
works. That's the way to go if you want to run it somewhere other than the
server, or build on the bot API.)

## Install

1. Build it, or download a release's `.tar`:

   ```sh
   ./build.sh                  # every platform
   ./build.sh linux-amd64      # or just the server's
   ```

   This gives you `dist/chriscord-music-plugin-<version>.tar`.

2. On the server machine, install **ffmpeg** with libopus (Debian/Ubuntu's
   `ffmpeg` package has it). For YouTube, also install **Deno**, because
   yt-dlp needs a JavaScript runtime there now:

   ```sh
   sudo apt install ffmpeg
   curl -fsSL https://deno.land/install.sh | sudo DENO_INSTALL=/usr/local sh
   ```

3. In the server's admin panel, open **Plugins → Install from a file** and
   choose the `.tar`. You can also unpack it into the server's `plugins/music/`
   folder; it then shows under "Found in the plugins folder".

4. The install screen shows what the plugin asks for: seeing channels,
   joining calls, speaking in them, and using the internet (it downloads
   what it plays). Choose:
   - the **voice channels** it may play in;
   - any **settings** you want to change.

   Then press Install. The plugin starts and joins the member list as
   "Music", tagged **PLUGIN**.

5. Add it to the server's **signed member list** (Server info → Member list
   in the app: it shows up under "Where the server and the list disagree").
   Until the approvers do, nobody's app sends it anything. It doesn't need
   to read any channel for this — only for chat commands (below).

yt-dlp downloads itself into the plugin's data folder on first start and
keeps itself up to date.

## Use

**The Music panel.** In the app, the music note in the channel header (or the
puzzle-piece tray next to it) opens the panel. It shows what's playing with
a progress bar, has Pause, Skip, Stop and Leave buttons, a box to paste a
link or search, and the queue (hover over a track to take it out). Anyone
can use it; the buttons that change what's playing need you to be in its
call.

**Commands.** Type them in the message box of a text channel:
`music::play never gonna give you up`. As you type, the app suggests the
commands, then each one's arguments, and colours the command. Tab or Enter
picks a suggestion. The command goes to the plugin end-to-end encrypted, and
is posted in the channel as your message, so everyone there sees what was
asked. What everyone should know, like "Queued …", the plugin announces in
the channel: shown there marked as the plugin's, but not a chat message (it
isn't kept in the channel's history). Problems and answers for you alone,
like `music::queue`, show only to you.

| Command | What it does |
|---|---|
| `music::play <link or search>` | Joins your call and plays it, or adds it to the queue |
| `music::search <words>`, then `music::pick <n>` | Choose from the top results |
| `music::np` | What's playing now |
| `music::pause`, `music::resume` | Pause and carry on |
| `music::skip` | Next track |
| `music::queue` | What's playing and what's next |
| `music::remove <n>` | Take a track out of the queue |
| `music::clear` | Empty the queue |
| `music::stop` | Stop and clear the queue |
| `music::leave` | Leave the call |
| `music::help` | All the commands |

The old chat commands (`!play`) work too, with **Chat commands too** turned
on in its settings — but then the plugin has to read the text channel, so
the signed member list's approvers have to let it, and whoever runs the
server can read that channel through it.

It leaves on its own after a few minutes alone in a call.

## Settings

Change these in the admin panel, under Plugins → Music. Saving restarts the
plugin.

- chat commands (`!play`) on or off, and their prefix;
- volume, bitrate, mono or stereo;
- download format;
- queue, playlist and track-length limits;
- how long it waits alone before leaving;
- how often it checks for a new yt-dlp;
- extra yt-dlp arguments;
- a yt-dlp or FFmpeg of your own.

For sites that need you signed in, put a `cookies.txt` in the plugin's data
folder (`plugin-data/music/` next to the server's database). Then set
**Extra yt-dlp arguments** to `--cookies cookies.txt`.

## Profile picture

Put `pfp.png` or `pfp.jpg` in `plugin-data/music/` and restart the plugin
from the admin panel. It's centre-cropped to a square and scaled to at most
512 px. A `pfp.png` next to `build.sh` is packed into the `.tar` as the
default picture.

## Privacy

Calls and chat stay end-to-end encrypted, and the plugin isn't part of
them: it plays into a call with its own keys, but nobody's app gives it the
call's keys, so it never hears anyone. It reads no channel unless you turn
on chat commands.

Commands, panel clicks, its answers, its panel and its announcements go
between members' apps and the plugin end-to-end encrypted ("lanes"): the
server passes them along without being able to read them. But the plugin
runs **on the server**, so whoever controls that machine can see what the
plugin sees — what people ask it, and what it plays. The server can also see
who uses it and when. Members can block it for themselves (the puzzle-piece
tray): their app then sends it nothing and hides what it shows.

On Linux the server runs it in a sandbox. It can only use its own data
folder, the system's programs (ffmpeg, Deno), this server, and web sites if
you allowed the internet. It can't use the server's files or other plugins.

## How it's started (for plugin authors)

The server runs `bin/music-<os>-<arch>` (the `run` list in `plugin.json`)
from the plugin's data folder, in the sandbox. It writes one line of JSON to
the program's stdin and then closes it:

```json
{"chriscord_plugin": 1, "id": "music", "server": "http://127.0.0.1:7070",
 "token": "ccbot_1_…", "settings": {"prefix": "!", …}, "data_dir": "…"}
```

`token` goes straight to the bot SDK (`chriscordbot.New`). `settings` holds
the values from `plugin.json`, already checked against their types and
limits. To stop the plugin, the server sends SIGTERM.

`plugin.json` lists its commands (with their arguments) and its panel. The
app's commands and panel clicks arrive on the bot connection as
`plugin_command` and `plugin_action` events. It answers with `plugin_reply`
and shows its panel with `plugin_panel` (`pluginui.go`, through the SDK's
`OnEvent` and `SendRaw`). `config.go` and `pluginui.go` are the
plugin-specific code; the rest is the same bot as chriscord-musicbot.

## Testing

```sh
go test ./...                                   # settings, the start-up line, links
go test -tags integration -v -run TestBot .     # the real plugin against a stand-in server
```

The integration test needs ffmpeg and yt-dlp (`BOT_YTDLP=/path/to/yt-dlp`,
or it fetches one). It runs the plugin against a stand-in server, with a
pretend member who types the commands and listens. It checks that:

- the plugin signs in with its certificate;
- the audio reaching the call decrypts with the key the plugin gave her,
  at real-time pace;
- the commands, the queue, and leaving when alone all work;
- the app's side works too: `music::` commands, answers only she sees, and
  the panel (what's playing, its buttons, its box, taking a track out).
