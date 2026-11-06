# Trakt watch-provider plugin for Silo

## Provides a fix for everything showing as COPS in trakt when scrobbled

Syncs Silo profiles with their [Trakt](https://trakt.tv) accounts through Silo's `watch_sync_provider.v1` plugin contract.

This is the ramindex fork of [Silo-Server/silo-plugin-watchprovider-trakt](https://github.com/Silo-Server/silo-plugin-watchprovider-trakt), which Silo archived when it dropped Trakt support. Silo's host refuses the original plugin ID, `silo.watchprovider.trakt`, so this fork ships as `silo.ramindex.trakt` from the ramindex plugin catalog. Silo registers it like any other third-party watch provider.

The plugin builds against `silo-plugin-sdk` v0.32.1 and needs a Silo server whose plugin host supports `silo-plugin-sdk` v0.21 or later, the release that added dropped shows to the contract.

## Capabilities

- Signs each profile in with Trakt's device code: Silo shows a code, and the profile owner enters it at trakt.tv/activate.
- Imports watched movies and episodes with their play counts and last watch times.
- Imports resume progress for movies and episodes.
- Imports favorites, the watchlist, and movie and show ratings as complete lists, so a title removed on Trakt is removed in Silo.
- Exports completed plays. Trakt stores every play it receives, repeats included, so before writing a batch the plugin reads the Trakt history around those plays and skips any play Trakt already has in the same minute.
- Exports unwatched titles. Trakt removes history by title, so this clears every play of the title.
- Adds and removes favorites and watchlist entries, and sets and clears movie and show ratings. Trakt uses the same 1–10 rating scale as the plugin contract.
- Syncs dropped shows both ways. Dropping a show on Trakt hides it from Up Next, progress, and the calendar without touching its history, and Trakt undrops a show on its own when it is watched again. The plugin imports Trakt's dropped shows as a complete list, and drops and undrops shows on Trakt in batches. Dropping a show that is already dropped, or undropping one that is not, succeeds without a change. Trakt's dropped list leaves out drops that apps make, Silo's included, so Silo keeps a drop it sent rather than reading the omission as an undrop.
- Sends live playback start, pause, and stop events. Trakt records a play when a stop arrives at 80% progress or later.

### How the plugin reads Trakt

Every sync reads the full Trakt lists; Trakt has no change feed the plugin could resume from. Trakt pages its lists, and a list that changes while it is read can shift a row between pages. A list longer than one page is therefore read twice, and the read only counts when both passes match. A list that changes during a sync is read again up to three times; if it keeps changing, or it changes after the plugin has already returned part of it to Silo, that sync stops and the next one starts over.

The plugin keeps to Trakt's rate limits for each connection: one write per second, and paged reads paced to stay under 500 per five minutes. When Trakt asks it to wait ten seconds or less, the plugin waits and retries; a longer wait, or no stated wait (five minutes), pauses that profile's sync until then.

## Setup

1. Create an API app at [trakt.tv/oauth/applications/new](https://trakt.tv/oauth/applications/new). Trakt requires a redirect URI; enter `urn:ietf:wg:oauth:2.0:oob`. Device sign-in does not use it.
2. Install the plugin in Silo, open its settings, and enter the app's **Client ID** and **Client secret**. Every profile on the server connects through this one app.
3. Each profile connects its own Trakt account from its watch-provider settings.

Until the client ID and secret are set, every Trakt request fails with a message asking an administrator to configure the plugin.

## Moving from Silo's Trakt support

Silo's Trakt removal deleted every Trakt connection and the saved client ID and secret, and disabled the original plugin. Watch history Silo imported from Trakt stays. After installing this plugin:

1. Enter the Trakt app's client ID and secret again (Setup, step 2). The existing Trakt API app works.
2. Each profile connects its Trakt account again.

The first sync reads each account in full, as every Trakt sync does. Silo records an imported play only when it is newer than the watch it already has, so history imported under the old provider is not doubled.

## Development

The plugin builds against `silo-plugin-sdk` v0.32.1.

```bash
make test
make build
./plugin manifest
```

`make build-all` produces static binaries for the platforms declared in `manifest.json`.

## Releasing

Releases follow the ramindex plugin catalog's `RELEASING.md`: bump `manifest.json`, push a `v<version>` tag, then update the catalog entry.

## License

AGPL-3.0-only.
