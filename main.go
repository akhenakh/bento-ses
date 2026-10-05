package main

import (
	"context"

	"github.com/warpstreamlabs/bento/public/service"

	// Import standard components.
	_ "github.com/warpstreamlabs/bento/public/components/all"

	// Import the custom plugin.
	_ "github.com/akhenakh/bento-ses/ses"
)

func main() {
	service.RunCLI(context.Background())
}
