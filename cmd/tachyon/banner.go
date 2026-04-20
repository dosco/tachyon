package main

import (
	"fmt"
	"strings"
)

const bannerArt = `  tachyon
  .--*-->
`

func renderBanner(listen, tls, quic string, workers int) string {
	var b strings.Builder
	b.WriteString(bannerArt)

	parts := []string{"listen " + listen}
	if tls != "" {
		parts = append(parts, "tls "+tls)
	}
	if quic != "" {
		parts = append(parts, "h3 "+quic)
	}
	if workers > 1 {
		parts = append(parts, fmt.Sprintf("%d workers", workers))
	}
	b.WriteString("  ")
	b.WriteString(strings.Join(parts, "  ·  "))
	b.WriteString("\n\n")
	return b.String()
}

func printBanner(listen, tls, quic string, workers int) {
	fmt.Print(renderBanner(listen, tls, quic, workers))
}
