package main

import (
	"github.com/jateeter/RealityEngine_GO/internal/server"
	"github.com/jateeter/RealityEngine_GO/internal/surface"
)

func main() {
	server.Main(surface.PE, "PERCEPTION_ENGINE_PORT")
}
