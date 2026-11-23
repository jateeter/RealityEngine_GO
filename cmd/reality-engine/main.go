package main

import (
	"github.com/jateeter/RealityEngine_GO/internal/server"
	"github.com/jateeter/RealityEngine_GO/internal/surface"
)

func main() {
	server.Main(surface.RE, "REALITY_ENGINE_PORT")
}
