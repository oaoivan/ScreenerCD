package launcher

import (
	"github.com/yourusername/screner/internal/config"
	"github.com/yourusername/screner/internal/dex/pricing"
	pb "github.com/yourusername/screner/pkg/protobuf"
)

// SupervisorFunc описывает функцию, которая оборачивает запуск коннектора в supervisor.
type SupervisorFunc func(name string, fn func() error)

// LaunchContext содержит «глобальные» зависимости, необходимые builder'ам для запуска коннекторов.
type LaunchContext struct {
	Config      *config.Config
	DataChannel chan<- *pb.MarketData
	Stop        <-chan struct{}
	Pricer      pricing.Pricer
	Supervisor  SupervisorFunc
}
