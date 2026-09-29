// Command mmqload measures the native WinCC OA path of an embedded broker
// (write -> WinCC OA -> hotlink -> subscriber). Create the load datapoints
// with winccoa/scripts/mmqCreateLoadDps.ctl first.
package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"monstermq.io/edge/internal/loadgen"
)

func main() {
	var cfg loadgen.Config
	prefix := flag.String("prefix", "MMQLoad", "datapoint name prefix (DPs are <prefix>00000..)")
	element := flag.String("element", "value", "float element written and read")
	flag.StringVar(&cfg.Broker, "broker", "tcp://127.0.0.1:1883", "broker URL")
	flag.StringVar(&cfg.Username, "user", "", "MQTT username")
	flag.StringVar(&cfg.Password, "pass", "", "MQTT password")
	flag.IntVar(&cfg.Subscribers, "clients", 50, "subscribing clients")
	flag.IntVar(&cfg.Writers, "writers", 4, "writing clients")
	flag.IntVar(&cfg.DPEs, "dpes", 5000, "number of datapoint elements")
	flag.IntVar(&cfg.Rate, "rate", 2000, "writes per second")
	flag.DurationVar(&cfg.Duration, "duration", time.Minute, "measurement duration")
	flag.DurationVar(&cfg.Settle, "settle", 3*time.Second, "wait for late values")
	flag.StringVar(&cfg.SubBroker, "sub-broker", "", "subscribe on this broker instead (e.g. a query bridge output)")
	flag.StringVar(&cfg.SubFilter, "sub-filter", "", "single filter every subscriber uses (e.g. ns/q/#)")
	flag.Parse()
	cfg.TopicFmt = loadgen.TopicFmtFor(*prefix, *element)
	res, err := loadgen.Run(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "mmqload:", err)
		os.Exit(1)
	}
	fmt.Println(res)
}
