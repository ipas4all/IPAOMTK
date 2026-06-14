# IPAOMTK Source Builder

Fast Go CLI that fetches every page from the IPAOMTK WordPress posts API and
converts the posts into an AltStore-style source JSON.

## Run

```sh
go run . -out ipaomtk-source.json
```

Useful flags:

```sh
go run . -max-pages 2 -out sample-source.json
go run . -concurrency 24 -out ipaomtk-source.json
go run . -bundle-prefix com.example.ipaomtk -out ipaomtk-source.json
go run . -pretty=false -out ipaomtk-source.min.json
go run . -inspect-ipa -inspect-max-apps 25 -out ipaomtk-source.json
```

## Field Mapping

- `downloads[0].download_url` -> `downloadURL`
- `downloads[0].download_version` -> `versions[0].version`
- `downloads[0].download_size` -> `versions[0].size`
- `downloads[0].download_mod_info` -> `subtitle` and version notes when present
- `yoast_head_json.description` -> `localizedDescription`
- featured media or Yoast image -> `iconURL`
- embedded categories or Yoast article sections -> `category`
- post publish date -> `versions[0].date`

The WordPress API does not expose the real iOS bundle identifier. The CLI
therefore creates a stable fallback from the post slug by default, such as
`com.ipaomtk.appdump`. Use `-bundle-prefix` to change that namespace.

For real bundle identifiers, pass `-inspect-ipa`. IPAs are ZIP files, so this
cannot reliably work from only the first bytes of the file. The inspector uses
HTTP range requests instead: it reads the ZIP tail, the central directory, and
the compressed `Payload/*.app/Info.plist` entry only. If the remote server does
not honor byte ranges, inspection fails closed instead of downloading the full
IPA.
