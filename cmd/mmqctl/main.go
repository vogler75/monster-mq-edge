package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
)

const usageText = `mmqctl - Command-line tool for MonsterMQ Edge Broker GraphQL Interface

Usage:
  mmqctl [global options] <command> [command args]

Global Options:
  --url URL           GraphQL endpoint URL (env: MQ_URL / GRAPHQL_URL, default: http://localhost:4000/graphql)
  --user USERNAME     Username for auth (env: MQ_USER / GRAPHQL_USER)
  --pass PASSWORD     Password for auth (env: MQ_PASS / GRAPHQL_PASS)
  --token TOKEN       JWT Bearer token (env: MQ_TOKEN / GRAPHQL_TOKEN)
  --env FILE          Path to .env file (default: .env)
  --json              Output raw JSON results

Commands:
  get-value <topic>                           Get current/retained value for a topic
  set-value <topic> <payload> [--retain]     Publish payload to a topic
  list-topics [pattern]                       Search/list active topics
  list-archives                               List all deployed archive groups
  archive-stats <group>                       Get stats for an archive group (--start, --end, --last-seconds)
  query-history <topic>                       Query historical messages for a topic
  query-aggregated <topics...>                Query aggregated time-series data
  features                                    List enabled broker features
  device list                                 List all configured devices/subsystems
  device download [name] [file]               Download device configuration JSON
  device upload <file>                        Upload device configuration JSON file
  device enable <name>                        Enable a device configuration
  device disable <name>                       Disable a device configuration

Examples:
  mmqctl --url http://localhost:4000/graphql get-value sensors/temp/room1
  mmqctl set-value sensors/temp/room1 '{"temp": 22.5}' --retain
  mmqctl query-history sensors/temp/room1 --last-seconds 3600
  mmqctl features
  mmqctl device list
  mmqctl device download MyDevice device.json
  mmqctl device upload device.json
`

func main() {
	if len(os.Args) < 2 {
		fmt.Print(usageText)
		os.Exit(1)
	}

	var flagURL, flagUser, flagPass, flagToken, envFile string
	var jsonMode, helpFlag bool

	fs := flag.NewFlagSet("mmqctl", flag.ContinueOnError)
	fs.StringVar(&flagURL, "url", "", "GraphQL endpoint URL")
	fs.StringVar(&flagUser, "user", "", "Username for authentication")
	fs.StringVar(&flagUser, "username", "", "Username for authentication")
	fs.StringVar(&flagPass, "pass", "", "Password for authentication")
	fs.StringVar(&flagPass, "password", "", "Password for authentication")
	fs.StringVar(&flagToken, "token", "", "JWT Bearer token")
	fs.StringVar(&envFile, "env", ".env", "Path to .env file")
	fs.StringVar(&envFile, "env-file", ".env", "Path to .env file")
	fs.BoolVar(&jsonMode, "json", false, "Output raw JSON")
	fs.BoolVar(&helpFlag, "help", false, "Show help usage")
	fs.BoolVar(&helpFlag, "h", false, "Show help usage")

	// Extract global flags vs subcommands
	var globalArgs []string
	var commandArgs []string

	for i := 1; i < len(os.Args); i++ {
		arg := os.Args[i]
		if strings.HasPrefix(arg, "-") && len(commandArgs) == 0 {
			globalArgs = append(globalArgs, arg)
			if (arg == "--url" || arg == "--user" || arg == "--username" || arg == "--pass" || arg == "--password" || arg == "--token" || arg == "--env" || arg == "--env-file") && i+1 < len(os.Args) {
				i++
				globalArgs = append(globalArgs, os.Args[i])
			}
		} else {
			commandArgs = append(commandArgs, arg)
		}
	}

	_ = fs.Parse(globalArgs)

	if helpFlag || len(commandArgs) == 0 {
		fmt.Print(usageText)
		os.Exit(0)
	}

	subcmd := commandArgs[0]
	subargs := commandArgs[1:]

	cfg := ResolveClientConfig(flagURL, flagUser, flagPass, flagToken, envFile, jsonMode)
	client := NewClient(cfg)
	ctx := context.Background()

	var err error
	switch subcmd {
	case "get-value", "get":
		err = runGetValue(ctx, client, subargs)
	case "set-value", "publish":
		err = runSetValue(ctx, client, subargs)
	case "list-topics", "search-topics":
		err = runListTopics(ctx, client, subargs)
	case "list-archives":
		err = runListArchives(ctx, client, subargs)
	case "archive-stats":
		err = runArchiveStats(ctx, client, subargs)
	case "query-history", "history":
		err = runQueryHistory(ctx, client, subargs)
	case "query-aggregated", "aggregated":
		err = runQueryAggregated(ctx, client, subargs)
	case "features", "enabled-features", "list-features":
		err = runListFeatures(ctx, client, subargs)
	case "device", "devices":
		if len(subargs) == 0 {
			err = runDeviceList(ctx, client, nil)
		} else {
			action := subargs[0]
			actionArgs := subargs[1:]
			switch action {
			case "list", "ls":
				err = runDeviceList(ctx, client, actionArgs)
			case "download", "export":
				err = runDeviceDownload(ctx, client, actionArgs)
			case "upload", "import":
				err = runDeviceUpload(ctx, client, actionArgs)
			case "enable", "start":
				err = runDeviceEnable(ctx, client, actionArgs)
			case "disable", "stop":
				err = runDeviceDisable(ctx, client, actionArgs)
			default:
				err = fmt.Errorf("unknown device action '%s' (use 'list', 'download', 'upload', 'enable', or 'disable')", action)
			}
		}
	default:
		err = fmt.Errorf("unknown command '%s'. Run 'mmqctl --help' for usage", subcmd)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}
