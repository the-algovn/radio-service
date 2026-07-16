# radio-service

Tần Số 42 (spec: the-algovn/specs products/radio.md). Phase 0 ships
`cmd/radio-lab` — the component bench behind the platform console
(web/apps/console). Run it via `dev/` Tilt. Keys: `~/.algovn/radio-lab.env`
(GOOGLE_TTS_API_KEY, GEMINI_API_KEY, ANTHROPIC_API_KEY — absent keys fall
back to fake providers).

## Phase-0 exit checklist (spec products/radio/lab.md)

Run the lab (`cd ~/the-algovn/dev && ./up`, console at
http://localhost:5174/console/) and close these with your ears:

- [ ] TTS voice chosen (voice audition) → record in specs products/radio/dj-brain.md
- [ ] LLM provider/model chosen (brain playground) → record in dj-brain.md
- [ ] persona v0 passes the ear test → commit persona/tieu-duong-duong.md
- [ ] yt-dlp validated: ≥10 searches + 3 downloads, ranking sane (ingest)
- [ ] one saved render that genuinely sounds like radio (mini-render) →
      record duck/offset/tail in specs products/radio/architecture.md defaults
- [ ] ≥10 call-in fixtures committed (internal/callin/testdata/fixtures/)
- [ ] ledger totals ≈ provider dashboards (±10%) — price constants verified
