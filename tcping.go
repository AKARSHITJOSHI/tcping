package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"math"
	"math/rand"
	"net"
	"net/netip"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"syscall"
	"time"

	"github.com/google/go-github/v45/github"
)

type stats struct {
	endTime               time.Time
	startOfUptime         time.Time
	startOfDowntime       time.Time
	lastSuccessfulProbe   time.Time
	lastUnsuccessfulProbe time.Time
	ip                    ipAddress
	startTime             time.Time
	statsPrinter
	retryHostnameResolveAfter uint // Retry resolving target's hostname after a certain number of failed requests
	hostname                  string
	rtt                       []float32
	ongoingUnsuccessfulProbes uint
	ongoingSuccessfulProbes   uint
	longestDowntime           longestTime
	totalSuccessfulProbes     uint
	totalUptime               time.Duration
	retriedHostnameResolves   uint
	longestUptime             longestTime
	totalDowntime             time.Duration
	totalUnsuccessfulProbes   uint
	port                      uint16
	wasDown                   bool // Used to determine the duration of a downtime
	isIP                      bool // If IP is provided instead of hostname, suppresses printing the IP information twice
	shouldRetryResolve        bool
	useIPv4                   bool
	useIPv6                   bool

	// ticker is used to handle time between probes.
	ticker *time.Ticker
}

type longestTime struct {
	start    time.Time
	end      time.Time
	duration time.Duration
}

type rttResults struct {
	min        float32
	max        float32
	average    float32
	hasResults bool
}

type replyMsg struct {
	msg string
	rtt float32
}

type ipAddress = netip.Addr
type cliArgs = []string
type calculatedTimeString = string

const (
	version    = "1.22.1"
	owner      = "pouriyajamshidi"
	repo       = "tcping"
	timeFormat = "2006-01-02 15:04:05"
	hourFormat = "15:04:05"
)

/* Catch SIGINT and print tcping stats */
func signalHandler(tcpStats *stats) {
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigChan
		totalRuntime := tcpStats.totalUnsuccessfulProbes + tcpStats.totalSuccessfulProbes
		tcpStats.endTime = tcpStats.startTime.Add(time.Duration(totalRuntime) * time.Second)
		tcpStats.printStatistics()
		os.Exit(0)
	}()
}

/* Print how program should be run */
func usage() {
	executableName := os.Args[0]

	colorLightCyan("\nTCPING version %s\n\n", version)
	colorRed("Try running %s like:\n", executableName)
	colorRed("%s <hostname/ip> <port number>. For example:\n", executableName)
	colorRed("%s www.example.com 443\n", executableName)
	colorYellow("\n[optional flags]\n")

	flag.VisitAll(func(f *flag.Flag) {
		flagName := f.Name
		if len(f.Name) > 1 {
			flagName = "-" + flagName
		}

		colorYellow("  -%s : %s\n", flagName, f.Usage)
	})

	os.Exit(1)
}

/* Get and validate user input */
func processUserInput(tcpStats *stats) {
	retryHostnameResolveAfter := flag.Uint("r", 0, "retry resolving target's hostname after <n> number of failed requests. e.g. -r 10 for 10 failed probes.")
	shouldCheckUpdates := flag.Bool("u", false, "check for updates.")
	outputJson := flag.Bool("j", false, "output in JSON format.")
	prettyJson := flag.Bool("pretty", false, "use indentation when using json output format. No effect without the -j flag.")
	showVersion := flag.Bool("v", false, "show version.")
	useIPv4 := flag.Bool("4", false, "use IPv4 only.")
	useIPv6 := flag.Bool("6", false, "use IPv6 only.")

	flag.CommandLine.Usage = usage

	permuteArgs(os.Args[1:])
	flag.Parse()

	/* validation for flag and args */
	args := flag.Args()
	nFlag := flag.NFlag()

	if *retryHostnameResolveAfter > 0 {
		tcpStats.retryHostnameResolveAfter = *retryHostnameResolveAfter
	}

	/* -u works on its own. */
	if *shouldCheckUpdates {
		if len(args) == 0 && nFlag == 1 {
			checkLatestVersion()
		} else {
			usage()
		}
	}

	if *showVersion {
		colorGreen("TCPING version %s\n", version)
		os.Exit(0)
	}

	if *useIPv4 && *useIPv6 {
		colorRed("Only one IP version can be specified\n")
		usage()
	}

	if *useIPv4 {
		tcpStats.useIPv4 = true
	}

	if *useIPv6 {
		tcpStats.useIPv6 = true
	}

	if *prettyJson {
		if !*outputJson {
			colorRed("--pretty has no effect without the -j flag.\n")
			usage()
		}

		jsonEncoder.SetIndent("", "\t")
	}

	/* host and port must be specified　*/
	if len(args) != 2 {
		usage()
	}

	/* the non-flag command-line arguments */
	port, err := strconv.ParseUint(args[1], 10, 16)

	if err != nil {
		colorRed("Invalid port number: %s\n", args[1])
		os.Exit(1)
	}

	if port < 1 || port > 65535 {
		colorRed("Port should be in 1..65535 range\n")
		os.Exit(1)
	}

	tcpStats.hostname = args[0]
	tcpStats.port = uint16(port)
	tcpStats.ip = resolveHostname(tcpStats)
	tcpStats.startTime = time.Now()

	if tcpStats.hostname == tcpStats.ip.String() {
		tcpStats.isIP = true
	}

	if tcpStats.retryHostnameResolveAfter > 0 && !tcpStats.isIP {
		tcpStats.shouldRetryResolve = true
	}

	/* output format determination. */
	if *outputJson {
		tcpStats.statsPrinter = &statsJsonPrinter{stats: tcpStats}
	} else {
		tcpStats.statsPrinter = &statsPlanePrinter{stats: tcpStats}
	}
}

/*
	Permute args for flag parsing stops just before the first non-flag argument.

see: https://pkg.go.dev/flag
*/
func permuteArgs(args cliArgs) {
	var flagArgs []string
	var nonFlagArgs []string

	for i := 0; i < len(args); i++ {
		v := args[i]
		if v[0] == '-' {
			optionName := v[1:]
			switch optionName {
			case "r":
				/* out of index */
				if len(args) <= i+1 {
					usage()
				}
				/* the next flag has come */
				optionVal := args[i+1]
				if optionVal[0] == '-' {
					usage()
				}
				flagArgs = append(flagArgs, args[i:i+2]...)
				i++
			default:
				flagArgs = append(flagArgs, args[i])
			}
		} else {
			nonFlagArgs = append(nonFlagArgs, args[i])
		}
	}
	permutedArgs := append(flagArgs, nonFlagArgs...)

	/* replace args */
	for i := 0; i < len(args); i++ {
		args[i] = permutedArgs[i]
	}
}

/* Check for updates and print messages if there is a newer version */
func checkLatestVersion() {
	c := github.NewClient(nil)

	/* unauthenticated requests from the same IP are limited to 60 per hour. */
	latestRelease, _, err := c.Repositories.GetLatestRelease(context.Background(), owner, repo)
	if err != nil {
		colorRed("Failed to check for updates %s\n", err.Error())
		os.Exit(1)
	}

	reg := `^v?(\d+\.\d+\.\d+)$`
	latestTagName := latestRelease.GetTagName()
	latestVersion := regexp.MustCompile(reg).FindStringSubmatch(latestTagName)

	if len(latestVersion) == 0 {
		colorRed("Failed to check for updates. The version name does not match the rule: %s\n", latestTagName)
		os.Exit(1)
	}

	if latestVersion[1] != version {
		colorLightBlue("Found newer version %s\n", latestVersion[1])
		colorLightBlue("Please update TCPING from the URL below:\n")
		colorLightBlue("https://github.com/%s/%s/releases/tag/%s\n", owner, repo, latestTagName)
	} else {
		colorLightBlue("Newer version not found. %s is the latest version.\n", version)
	}
	os.Exit(0)
}

/* Hostname resolution */
func resolveHostname(tcpStats *stats) ipAddress {
	ip, err := netip.ParseAddr(tcpStats.hostname)
	if err == nil {
		return ip
	}

	ipAddrs, err := net.LookupIP(tcpStats.hostname)

	if err != nil && (tcpStats.totalSuccessfulProbes != 0 || tcpStats.totalUnsuccessfulProbes != 0) {
		/* Prevent exit if application has been running for a while */
		return tcpStats.ip
	} else if err != nil {
		colorRed("Failed to resolve %s\n", tcpStats.hostname)
		os.Exit(1)
	}

	var index int
	var ipList []net.IP

	switch {
	case tcpStats.useIPv4:
		for _, ip := range ipAddrs {
			if ip.To4() != nil {
				ipList = append(ipList, ip)
			}
		}
		if len(ipList) == 0 {
			colorRed("Failed to find IPv4 address for %s\n", tcpStats.hostname)
			os.Exit(1)
		}
		if len(ipList) > 1 {
			index = rand.Intn(len(ipAddrs))
		} else {
			index = 0
		}
		ip, _ = netip.ParseAddr(ipList[index].String())

	case tcpStats.useIPv6:
		for _, ip := range ipAddrs {
			if ip.To16() != nil {
				ipList = append(ipList, ip)
			}
		}
		if len(ipList) == 0 {
			colorRed("Failed to find IPv6 address for %s\n", tcpStats.hostname)
			os.Exit(1)
		}
		if len(ipList) > 1 {
			index = rand.Intn(len(ipAddrs))
		} else {
			index = 0
		}
		ip, _ = netip.ParseAddr(ipList[index].String())

	default:
		if len(ipAddrs) > 1 {
			index = rand.Intn(len(ipAddrs))
		} else {
			index = 0
		}
		ip, _ = netip.ParseAddr(ipAddrs[index].String())
	}

	return ip
}

/* Retry resolve hostname after certain number of failures */
func retryResolve(tcpStats *stats) {
	if tcpStats.ongoingUnsuccessfulProbes >= tcpStats.retryHostnameResolveAfter {
		tcpStats.printRetryingToResolve()
		tcpStats.ip = resolveHostname(tcpStats)
		tcpStats.ongoingUnsuccessfulProbes = 0
		tcpStats.retriedHostnameResolves += 1
	}
}

/* Create LongestTime structure */
func newLongestTime(startTime time.Time, duration time.Duration) longestTime {
	return longestTime{
		start:    startTime,
		end:      startTime.Add(duration),
		duration: duration,
	}
}

/* Find min/avg/max RTT values. The last int acts as err code */
func findMinAvgMaxRttTime(timeArr []float32) rttResults {
	var accum float32
	var rttResults rttResults

	arrLen := len(timeArr)
	// rttResults.min = ^uint(0.0)
	if arrLen > 0 {
		rttResults.min = timeArr[0]
	}

	for i := 0; i < arrLen; i++ {
		accum += timeArr[i]

		if timeArr[i] > rttResults.max {
			rttResults.max = timeArr[i]
		}

		if timeArr[i] < rttResults.min {
			rttResults.min = timeArr[i]
		}
	}

	if arrLen > 0 {
		rttResults.hasResults = true
		rttResults.average = accum / float32(arrLen)
	}

	return rttResults
}

// calcTime creates a human-readable string for a given duration
func calcTime(duration time.Duration) calculatedTimeString {
	hours := math.Floor(duration.Hours())
	if hours > 0 {
		duration -= time.Duration(hours * float64(time.Hour))
	}

	minutes := math.Floor(duration.Minutes())
	if minutes > 0 {
		duration -= time.Duration(minutes * float64(time.Minute))
	}

	seconds := duration.Seconds()

	switch {
	// Hours
	case hours >= 2:
		return fmt.Sprintf("%.0f hours %.0f minutes %.0f seconds", hours, minutes, seconds)
	case hours == 1 && minutes == 0 && seconds == 0:
		return fmt.Sprintf("%.0f hour", hours)
	case hours == 1:
		return fmt.Sprintf("%.0f hour %.0f minutes %.0f seconds", hours, minutes, seconds)

	// Minutes
	case minutes >= 2:
		return fmt.Sprintf("%.0f minutes %.0f seconds", minutes, seconds)
	case minutes == 1 && seconds == 0:
		return fmt.Sprintf("%.0f minute", minutes)
	case minutes == 1:
		return fmt.Sprintf("%.0f minute %.0f seconds", minutes, seconds)

	// Seconds
	case seconds == 1:
		return fmt.Sprintf("%.0f second", seconds)
	default:
		return fmt.Sprintf("%.0f seconds", seconds)

	}
}

// calcLongestUptime calculates the longest uptime and sets it to tcpStats.
func calcLongestUptime(tcpStats *stats, duration time.Duration) {
	if tcpStats.startOfUptime.IsZero() {
		return
	}

	longestUptime := newLongestTime(tcpStats.startOfUptime, duration)

	// It means it is the first time we're calling this function
	if tcpStats.longestUptime.end.IsZero() {
		tcpStats.longestUptime = longestUptime
		return
	}

	if longestUptime.duration >= tcpStats.longestUptime.duration {
		tcpStats.longestUptime = longestUptime
	}
}

// calcLongestDowntime calculates the longest downtime and sets it to tcpStats.
func calcLongestDowntime(tcpStats *stats, duration time.Duration) {
	if tcpStats.startOfDowntime.IsZero() {
		return
	}

	longestDowntime := newLongestTime(tcpStats.startOfDowntime, duration)

	// It means it is the first time we're calling this function
	if tcpStats.longestDowntime.end.IsZero() {
		tcpStats.longestDowntime = longestDowntime
		return
	}

	if longestDowntime.duration >= tcpStats.longestDowntime.duration {
		tcpStats.longestDowntime = longestDowntime
	}
}

// nanoToMillisecond returns an amount of milliseconds from nanoseconds.
// Using duration.Milliseconds() is not an option, because it drops
// decimal points, returning an int.
func nanoToMillisecond(nano int64) float32 {
	return float32(nano) / float32(time.Millisecond)
}

func (tcpStats *stats) handleConnError(now time.Time) {
	if !tcpStats.wasDown {
		tcpStats.startOfDowntime = now
		calcLongestUptime(tcpStats,
			time.Duration(tcpStats.ongoingSuccessfulProbes)*time.Second)
		tcpStats.startOfUptime = time.Time{}
		tcpStats.wasDown = true
	}

	tcpStats.totalDowntime += time.Second
	tcpStats.lastUnsuccessfulProbe = now
	tcpStats.totalUnsuccessfulProbes += 1
	tcpStats.ongoingUnsuccessfulProbes += 1

	tcpStats.statsPrinter.printReply(replyMsg{msg: "No reply", rtt: 0})
}

func (tcpStats *stats) handleConnSuccess(rtt float32, now time.Time) {
	if tcpStats.wasDown {
		tcpStats.statsPrinter.printTotalDownTime(now)
		tcpStats.startOfUptime = now
		calcLongestDowntime(tcpStats,
			time.Duration(tcpStats.ongoingUnsuccessfulProbes)*time.Second)
		tcpStats.startOfDowntime = time.Time{}
		tcpStats.wasDown = false
		tcpStats.ongoingUnsuccessfulProbes = 0
		tcpStats.ongoingSuccessfulProbes = 0
	}

	if tcpStats.startOfUptime.IsZero() {
		tcpStats.startOfUptime = now
	}

	tcpStats.totalUptime += time.Second
	tcpStats.lastSuccessfulProbe = now
	tcpStats.totalSuccessfulProbes += 1
	tcpStats.ongoingSuccessfulProbes += 1
	tcpStats.rtt = append(tcpStats.rtt, rtt)

	tcpStats.statsPrinter.printReply(replyMsg{msg: "Reply", rtt: rtt})
}

/* Ping host, TCP style */
func tcping(tcpStats *stats) {
	IPAndPort := netip.AddrPortFrom(tcpStats.ip, tcpStats.port)

	connStart := time.Now()
	conn, err := net.DialTimeout("tcp", IPAndPort.String(), time.Second)
	connEnd := time.Since(connStart)
	rtt := nanoToMillisecond(connEnd.Nanoseconds())
	now := time.Now()

	if err != nil {
		tcpStats.handleConnError(now)
	} else {
		tcpStats.handleConnSuccess(rtt, now)
		conn.Close()
	}

	<-tcpStats.ticker.C
}

/* Capture keystrokes from stdin */
func monitorStdin(stdinChan chan string) {
	reader := bufio.NewReader(os.Stdin)
	for {
		key, _ := reader.ReadString('\n')
		stdinChan <- key
	}
}

func main() {
	tcpStats := &stats{
		ticker: time.NewTicker(time.Second),
	}
	defer tcpStats.ticker.Stop()
	processUserInput(tcpStats)
	signalHandler(tcpStats)
	tcpStats.printStart()

	stdinChan := make(chan string)
	go monitorStdin(stdinChan)

	for {
		if tcpStats.shouldRetryResolve {
			retryResolve(tcpStats)
		}

		tcping(tcpStats)

		/* print stats when the `enter` key is pressed */
		select {
		case stdin := <-stdinChan:
			if stdin == "\n" || stdin == "\r" || stdin == "\r\n" {
				tcpStats.printStatistics()
			}
		default:
			continue
		}
	}
}
