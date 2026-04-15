package main

import (
	"github.com/pion/logging"
	goxash3d_fwgs "github.com/yohimik/goxash3d-fwgs/pkg"
	"time"
)

var log = logging.NewDefaultLoggerFactory().NewLogger("main")

func main() {
	goxash3d_fwgs.DefaultXash3D.Net = sfuNet

	// Teste de vida do servidor
	go func() {
		for {
			log.Infof("SERVER STILL RUNNING")
			time.Sleep(10 * time.Second)
		}
	}()

	go runSFU()

	goxash3d_fwgs.DefaultXash3D.SysStart()
}
