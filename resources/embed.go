package resources

import _ "embed"

// ConsolePage is the single-page management console served at
// /v0/resource/plugins/<pluginID>/console.
//
//go:embed index.html
var ConsolePage string

// AuthJS is the console auth module injected into the page at the
// /*__AUTH_MODULE__*/ placeholder. It mirrors the ClinePassBridge console
// module: reuse the remembered CPA / CPAMP login, keep manual keys per the
// official panel's storage rules, and clear a rejected key.
//
//go:embed auth.js
var AuthJS string
