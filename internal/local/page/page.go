// Package page holds Local MTProto's single-file web app: the same page as
// MTProto Remote's, built for one proxy on the router itself.
package page

import "embed"

// The image build adds index.html.br; the repo keeps only a text placeholder.
//
//go:embed index.html*
var Files embed.FS
