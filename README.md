# q2-soulseek

A Soulseek client for the **Shanling Q2** running [Q2 Pod](https://github.com/DiamondBond/q2-pod). Search the Soulseek network and download music straight to the card, either on the Q2 with the wheel, or from your phone on the same Wi-Fi, on a page laid out like Nicotine+. Sharing is off until you turn it on, and then, by default, happens only while a cable powers the Q2. It needs a Q2 Pod build that supports card apps (its **System settings → Apps** lists Soulseek).

## Install

**Over Wi-Fi:** on the Q2, open **System settings → Apps** and press **Soulseek**.

**By hand:**

1. Download `q2-soulseek-*.zip` from the [latest release](https://github.com/group-a-study/q2-soulseek/releases/latest).
2. Power the Q2 off, then put its microSD card in your computer.
3. Unzip the file to the card's root, so the card has `.soulseek/q2-soulseek`, `.soulseek/run`, `.soulseek/q2app` and its icons `.soulseek/icon.png` and `.soulseek/icon-40.png`. The leading dot hides the folder on macOS (Cmd+Shift+. in Finder shows it) and Linux.
4. Eject the card, put it back in the Q2 and power it on.

Soulseek then has a row in **Streaming**, with its logo.

## Set up

Open **Streaming → Soulseek** and press **Set up on your phone**: the Q2 shows a QR code, its address and a PIN. Scan the code with your phone's camera (or open the address and type the PIN), then pick a Soulseek username and password on the page that opens. Soulseek creates the account the first time a new name signs in. There is no password reset, so keep the password.

## Use

**On the Q2:** Soulseek's page has **Phone Remote** (the QR code again), **Search**, **Downloads**, **Uploads**, **Sharing**, **Account** and **Stop Soulseek**. Search uses the Q2's keyboard and lists the folders (albums) its audio files are in, 50 a page with **More…** for the next, each with its user, number of files, format, size and whether a slot is free. The ones with a free slot and the fastest come first, and a folder keeps its place while results come in. Press a folder to see its files under **Download Folder**, and press a file to download it; hold **Play/Pause** on a folder or a file to download the whole folder. Downloads and uploads refresh every two seconds while their page is open, and a download's row offers pause, resume, retry and remove.

**On your phone:** `http://<the Q2's address>:8080/` has **Search** (results grouped by user and folder, the fastest with a free slot first, filters for words, lossless, FLAC, MP3, bitrate and free slots; **Download folder** takes a whole album; the first 200 files show, **Show more** adds the next; once the screen is full, results that come in later wait behind **Show N new results**, so the list doesn't move under your finger), **Downloads** (by album, with progress, speed and time left), **Uploads** and **Settings** (account, sharing, shared folders, upload slots and speed, the download folder, when to stop by itself, the listening port).

Finished downloads go to `Soulseek/<album folder>/` on the card; run **Library → Update Local Music** to play them. Unfinished ones wait in `Soulseek/.incomplete/` and carry on where they stopped, even after a restart.

## Sharing and the battery

| Sharing                                 | Shares when                                                             |
| --------------------------------------- | ----------------------------------------------------------------------- |
| **Off** (the default)                   | never: download only                                                    |
| **Only while charging**                 | a cable powers the Q2 (Charge Limit holding the battery at 80% counts)  |
| **While charging, or battery above N%** | as above, or on battery while it is at least N% (80 by default)         |

The level is the one the Q2's status bar shows, and the phone page's top bar shows it too.

Unplugged (or below the level), sharing stops at once and queued uploads wait for the next charge. By default the card's top-level folders are shared, except Photos, Books, Videos, EQ, Radio and the system folders, plus the download folder. Hidden files are never shared.

The Q2 stays awake while files move, while downloads wait for a slot (up to 30 minutes on battery), while the phone page is in use and, when sharing on a cable, all the time; the screen still turns off. On battery, with nothing to do and neither the phone nor the Q2's page used for 30 minutes (Settings: 15, 30, 60 or never), Soulseek stops by itself. **Streaming → Soulseek** starts it again.

## Update and remove

**System settings → Apps** shows **Update** when a newer release is out. By hand, replace `.soulseek/q2-soulseek`, `.soulseek/run`, `.soulseek/q2app`, `.soulseek/icon.png` and `.soulseek/icon-40.png`. Either way, `.soulseek/config.json` (your account and settings; it holds the password as typed, as Soulseek clients do), `.soulseek/downloads.json` and `.soulseek/shares.cache` are kept.

To remove it, press **Soulseek** twice in **System settings → Apps**, or delete `.soulseek` to start over completely. Your downloads in `Soulseek/` stay.

## Build

    contrib/shanlingq2/build.sh

needs Go 1.22 or later and python3 (no dependencies outside Go's standard library), and leaves the card files in `target/shanlingq2/.soulseek` and the release ZIP beside them. Pushing a tag `q2-vX.Y` that matches `cmd/q2-soulseek/main.go`'s version builds and publishes the release (`.github/workflows/release.yml`). How it works: [contrib/shanlingq2/README](contrib/shanlingq2/README).

## License

[MIT](LICENSE). The QR encoder (`internal/qr`) follows Project Nayuki's QR Code generator (MIT).
