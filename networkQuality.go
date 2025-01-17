/*
 * This file is part of Go Responsiveness.
 *
 * Go Responsiveness is free software: you can redistribute it and/or modify it under
 * the terms of the GNU General Public License as published by the Free Software Foundation,
 * either version 2 of the License, or (at your option) any later version.
 * Go Responsiveness is distributed in the hope that it will be useful, but WITHOUT ANY
 * WARRANTY; without even the implied warranty of MERCHANTABILITY or FITNESS FOR A
 * PARTICULAR PURPOSE. See the GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License along
 * with Go Responsiveness. If not, see <https://www.gnu.org/licenses/>.
 */

package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"net/url"
	"os"
	"runtime/pprof"
	"time"

	"github.com/network-quality/goresponsiveness/ccw"
	"github.com/network-quality/goresponsiveness/config"
	"github.com/network-quality/goresponsiveness/constants"
	"github.com/network-quality/goresponsiveness/datalogger"
	"github.com/network-quality/goresponsiveness/debug"
	"github.com/network-quality/goresponsiveness/extendedstats"
	"github.com/network-quality/goresponsiveness/lgc"
	"github.com/network-quality/goresponsiveness/ms"
	"github.com/network-quality/goresponsiveness/probe"
	"github.com/network-quality/goresponsiveness/qualityattenuation"
	"github.com/network-quality/goresponsiveness/rpm"
	"github.com/network-quality/goresponsiveness/stabilizer"
	"github.com/network-quality/goresponsiveness/timeoutat"
	"github.com/network-quality/goresponsiveness/utilities"
)

var (
	// Variables to hold CLI arguments.
	configHost = flag.String(
		"config",
		constants.DefaultConfigHost,
		"name/IP of responsiveness configuration server.",
	)
	configPort = flag.Int(
		"port",
		constants.DefaultPortNumber,
		"port number on which to access responsiveness configuration server.",
	)
	configPath = flag.String(
		"path",
		"config",
		"path on the server to the configuration endpoint.",
	)
	configURL = flag.String(
		"url",
		"",
		"configuration URL (takes precedence over other configuration parts)",
	)
	debugCliFlag = flag.Bool(
		"debug",
		constants.DefaultDebug,
		"Enable debugging.",
	)
	rpmtimeout = flag.Int(
		"rpmtimeout",
		constants.RPMCalculationTime,
		"Maximum time to spend calculating RPM (i.e., total test time.).",
	)
	sslKeyFileName = flag.String(
		"ssl-key-file",
		"",
		"Store the per-session SSL key files in this file.",
	)
	profile = flag.String(
		"profile",
		"",
		"Enable client runtime profiling and specify storage location. Disabled by default.",
	)
	calculateExtendedStats = flag.Bool(
		"extended-stats",
		false,
		"Enable the collection and display of extended statistics -- may not be available on certain platforms.",
	)
	printQualityAttenuation = flag.Bool(
		"quality-attenuation",
		false,
		"Print quality attenuation information.",
	)
	dataLoggerBaseFileName = flag.String(
		"logger-filename",
		"",
		"Store granular information about tests results in files with this basename. Time and information type will be appended (before the first .) to create separate log files. Disabled by default.",
	)
	probeIntervalTime = flag.Uint(
		"probe-interval-time",
		100,
		"Time (in ms) between probes (foreign and self).",
	)
	connectToAddr = flag.String(
		"connect-to",
		"",
		"address (hostname or IP) to connect to (overriding DNS). Disabled by default.",
	)
	insecureSkipVerify = flag.Bool(
		"insecure-skip-verify",
		constants.DefaultInsecureSkipVerify,
		"Enable server certificate validation.",
	)
	prometheusStatsFilename = flag.String(
		"prometheus-stats-filename",
		"",
		"If filename specified, prometheus stats will be written. If specified file exists, it will be overwritten.",
	)
	showVersion = flag.Bool(
		"version",
		false,
		"Show version.",
	)
)

func main() {
	flag.Parse()

	if *showVersion {
		fmt.Fprintf(os.Stdout, "goresponsiveness %s\n", utilities.GitVersion)
		os.Exit(0)
	}

	timeoutDuration := time.Second * time.Duration(*rpmtimeout)
	timeoutAbsoluteTime := time.Now().Add(timeoutDuration)

	var configHostPort string

	// if user specified a full URL, use that and set the various parts we need out of it
	if len(*configURL) > 0 {
		parsedURL, err := url.ParseRequestURI(*configURL)
		if err != nil {
			fmt.Printf("Error: Could not parse %q: %s", *configURL, err)
			os.Exit(1)
		}

		*configHost = parsedURL.Hostname()
		*configPath = parsedURL.Path
		// We don't explicitly care about configuring the *configPort.
		configHostPort = parsedURL.Host // host or host:port
	} else {
		configHostPort = fmt.Sprintf("%s:%d", *configHost, *configPort)
	}

	// This is the overall operating context of the program. All other
	// contexts descend from this one. Canceling this one cancels all
	// the others.
	operatingCtx, operatingCtxCancel := context.WithCancel(context.Background())

	// The operator contexts. These contexts control the processes that manage
	// network activity but do no control network activity.

	uploadLoadGeneratorOperatorCtx, uploadLoadGeneratorOperatorCtxCancel := context.WithCancel(operatingCtx)
	downloadLoadGeneratorOperatorCtx, downloadLoadGeneratorOperatorCtxCancel := context.WithCancel(operatingCtx)
	proberOperatorCtx, proberOperatorCtxCancel := context.WithCancel(operatingCtx)

	// This context is used to control the network activity (i.e., it controls all
	// the connections that are open to do load generation and probing). Cancelling this context will close
	// all the network connections that are responsible for generating the load.
	networkActivityCtx, networkActivityCtxCancel := context.WithCancel(operatingCtx)

	config := &config.Config{
		ConnectToAddr: *connectToAddr,
	}
	var debugLevel debug.DebugLevel = debug.Error

	if *debugCliFlag {
		debugLevel = debug.Debug
	}

	if *calculateExtendedStats && !extendedstats.ExtendedStatsAvailable() {
		*calculateExtendedStats = false
		fmt.Printf(
			"Warning: Calculation of extended statistics was requested but is not supported on this platform.\n",
		)
	}

	var sslKeyFileConcurrentWriter *ccw.ConcurrentWriter = nil
	if *sslKeyFileName != "" {
		if sslKeyFileHandle, err := os.OpenFile(*sslKeyFileName, os.O_RDWR|os.O_CREATE, os.FileMode(0600)); err != nil {
			fmt.Printf("Could not open the requested SSL key logging file for writing: %v!\n", err)
			sslKeyFileConcurrentWriter = nil
		} else {
			if err = utilities.SeekForAppend(sslKeyFileHandle); err != nil {
				fmt.Printf("Could not seek to the end of the SSL key logging file: %v!\n", err)
				sslKeyFileConcurrentWriter = nil
			} else {
				if debug.IsDebug(debugLevel) {
					fmt.Printf("Doing SSL key logging through file %v\n", *sslKeyFileName)
				}
				sslKeyFileConcurrentWriter = ccw.NewConcurrentFileWriter(sslKeyFileHandle)
				defer sslKeyFileHandle.Close()
			}
		}
	}

	if err := config.Get(configHostPort, *configPath, *insecureSkipVerify, sslKeyFileConcurrentWriter); err != nil {
		fmt.Fprintf(os.Stderr, "%s\n", err)
		os.Exit(1)
	}
	if err := config.IsValid(); err != nil {
		fmt.Fprintf(
			os.Stderr,
			"Error: Invalid configuration returned from %s: %v\n",
			config.Source,
			err,
		)
		os.Exit(1)
	}
	if debug.IsDebug(debugLevel) {
		fmt.Printf("Configuration: %s\n", config)
	}

	timeoutChannel := timeoutat.TimeoutAt(
		operatingCtx,
		timeoutAbsoluteTime,
		debugLevel,
	)
	if debug.IsDebug(debugLevel) {
		fmt.Printf("Test will end no later than %v\n", timeoutAbsoluteTime)
	}

	// print the banner
	dt := time.Now().UTC()
	fmt.Printf(
		"%s UTC Go Responsiveness to %s...\n",
		dt.Format("01-02-2006 15:04:05"),
		configHostPort,
	)

	if len(*profile) != 0 {
		f, err := os.Create(*profile)
		if err != nil {
			fmt.Fprintf(
				os.Stderr,
				"Error: Profiling requested but could not open the log file ( %s ) for writing: %v\n",
				*profile,
				err,
			)
			os.Exit(1)
		}
		pprof.StartCPUProfile(f)
		defer pprof.StopCPUProfile()
	}
	var selfProbeDataLogger datalogger.DataLogger[probe.ProbeDataPoint] = nil
	var foreignProbeDataLogger datalogger.DataLogger[probe.ProbeDataPoint] = nil
	var downloadThroughputDataLogger datalogger.DataLogger[rpm.ThroughputDataPoint] = nil
	var uploadThroughputDataLogger datalogger.DataLogger[rpm.ThroughputDataPoint] = nil
	var granularThroughputDataLogger datalogger.DataLogger[rpm.GranularThroughputDataPoint] = nil

	// User wants to log data
	if *dataLoggerBaseFileName != "" {
		var err error = nil
		unique := time.Now().UTC().Format("01-02-2006-15-04-05")

		dataLoggerSelfFilename := utilities.FilenameAppend(*dataLoggerBaseFileName, "-self-"+unique)
		dataLoggerForeignFilename := utilities.FilenameAppend(
			*dataLoggerBaseFileName,
			"-foreign-"+unique,
		)
		dataLoggerDownloadThroughputFilename := utilities.FilenameAppend(
			*dataLoggerBaseFileName,
			"-throughput-download-"+unique,
		)
		dataLoggerUploadThroughputFilename := utilities.FilenameAppend(
			*dataLoggerBaseFileName,
			"-throughput-upload-"+unique,
		)
		dataLoggerGranularThroughputFilename := utilities.FilenameAppend(
			*dataLoggerBaseFileName,
			"-throughput-granular-"+unique,
		)

		selfProbeDataLogger, err = datalogger.CreateCSVDataLogger[probe.ProbeDataPoint](
			dataLoggerSelfFilename,
		)
		if err != nil {
			fmt.Printf(
				"Warning: Could not create the file for storing self probe results (%s). Disabling functionality.\n",
				dataLoggerSelfFilename,
			)
			selfProbeDataLogger = nil
		}

		foreignProbeDataLogger, err = datalogger.CreateCSVDataLogger[probe.ProbeDataPoint](
			dataLoggerForeignFilename,
		)
		if err != nil {
			fmt.Printf(
				"Warning: Could not create the file for storing foreign probe results (%s). Disabling functionality.\n",
				dataLoggerForeignFilename,
			)
			foreignProbeDataLogger = nil
		}

		downloadThroughputDataLogger, err = datalogger.CreateCSVDataLogger[rpm.ThroughputDataPoint](
			dataLoggerDownloadThroughputFilename,
		)
		if err != nil {
			fmt.Printf(
				"Warning: Could not create the file for storing download throughput results (%s). Disabling functionality.\n",
				dataLoggerDownloadThroughputFilename,
			)
			downloadThroughputDataLogger = nil
		}

		uploadThroughputDataLogger, err = datalogger.CreateCSVDataLogger[rpm.ThroughputDataPoint](
			dataLoggerUploadThroughputFilename,
		)
		if err != nil {
			fmt.Printf(
				"Warning: Could not create the file for storing upload throughput results (%s). Disabling functionality.\n",
				dataLoggerUploadThroughputFilename,
			)
			uploadThroughputDataLogger = nil
		}

		granularThroughputDataLogger, err = datalogger.CreateCSVDataLogger[rpm.GranularThroughputDataPoint](
			dataLoggerGranularThroughputFilename,
		)
		if err != nil {
			fmt.Printf(
				"Warning: Could not create the file for storing granular throughput results (%s). Disabling functionality.\n",
				dataLoggerGranularThroughputFilename,
			)
			granularThroughputDataLogger = nil
		}
	}
	// If, for some reason, the data loggers are nil, make them Null Data Loggers so that we don't have conditional
	// code later.
	if selfProbeDataLogger == nil {
		selfProbeDataLogger = datalogger.CreateNullDataLogger[probe.ProbeDataPoint]()
	}
	if foreignProbeDataLogger == nil {
		foreignProbeDataLogger = datalogger.CreateNullDataLogger[probe.ProbeDataPoint]()
	}
	if downloadThroughputDataLogger == nil {
		downloadThroughputDataLogger = datalogger.CreateNullDataLogger[rpm.ThroughputDataPoint]()
	}
	if uploadThroughputDataLogger == nil {
		uploadThroughputDataLogger = datalogger.CreateNullDataLogger[rpm.ThroughputDataPoint]()
	}
	if granularThroughputDataLogger == nil {
		granularThroughputDataLogger = datalogger.CreateNullDataLogger[rpm.GranularThroughputDataPoint]()
	}

	/*
	 * Create (and then, ironically, name) two anonymous functions that, when invoked,
	 * will create load-generating connections for upload/download
	 */
	generateLgdc := func() lgc.LoadGeneratingConnection {
		lgd := lgc.NewLoadGeneratingConnectionDownload(config.Urls.LargeUrl, sslKeyFileConcurrentWriter, config.ConnectToAddr, *insecureSkipVerify)
		return &lgd
	}

	generateLguc := func() lgc.LoadGeneratingConnection {
		lgu := lgc.NewLoadGeneratingConnectionUpload(config.Urls.UploadUrl, sslKeyFileConcurrentWriter, config.ConnectToAddr, *insecureSkipVerify)
		return &lgu
	}

	generateSelfProbeConfiguration := func() probe.ProbeConfiguration {
		return probe.ProbeConfiguration{
			URL:                config.Urls.SmallUrl,
			ConnectToAddr:      config.ConnectToAddr,
			InsecureSkipVerify: *insecureSkipVerify,
		}
	}

	generateForeignProbeConfiguration := func() probe.ProbeConfiguration {
		return probe.ProbeConfiguration{
			URL:                config.Urls.SmallUrl,
			ConnectToAddr:      config.ConnectToAddr,
			InsecureSkipVerify: *insecureSkipVerify,
		}
	}

	var downloadDebugging *debug.DebugWithPrefix = debug.NewDebugWithPrefix(debugLevel, "download")
	var uploadDebugging *debug.DebugWithPrefix = debug.NewDebugWithPrefix(debugLevel, "upload")
	var combinedProbeDebugging *debug.DebugWithPrefix = debug.NewDebugWithPrefix(debugLevel, "combined probe")

	downloadLoadGeneratingConnectionCollection := lgc.NewLoadGeneratingConnectionCollection()
	uploadLoadGeneratingConnectionCollection := lgc.NewLoadGeneratingConnectionCollection()

	// TODO: Separate contexts for load generation and data collection. If we do that, if either of the two
	// data collection go routines stops well before the other, they will continue to send probes and we can
	// generate additional information!

	selfDownProbeConnectionCommunicationChannel, downloadThroughputChannel := rpm.LoadGenerator(
		networkActivityCtx,
		downloadLoadGeneratorOperatorCtx,
		time.Second,
		generateLgdc,
		&downloadLoadGeneratingConnectionCollection,
		*calculateExtendedStats,
		downloadDebugging,
	)
	selfUpProbeConnectionCommunicationChannel, uploadThroughputChannel := rpm.LoadGenerator(
		networkActivityCtx,
		uploadLoadGeneratorOperatorCtx,
		time.Second,
		generateLguc,
		&uploadLoadGeneratingConnectionCollection,
		*calculateExtendedStats,
		uploadDebugging,
	)

	// Handles for the first connection that the load-generating go routines (both up and
	// download) open are passed back on the self[Down|Up]ProbeConnectionCommunicationChannel
	// so that we can then start probes on those connections.
	selfDownProbeConnection := <-selfDownProbeConnectionCommunicationChannel
	selfUpProbeConnection := <-selfUpProbeConnectionCommunicationChannel

	// The combined prober will handle launching, monitoring, etc of *both* the self and foreign
	// probes.
	probeDataPointsChannel := rpm.CombinedProber(
		proberOperatorCtx,
		networkActivityCtx,
		generateForeignProbeConfiguration,
		generateSelfProbeConfiguration,
		selfDownProbeConnection,
		selfUpProbeConnection,
		time.Millisecond*(time.Duration(*probeIntervalTime)),
		sslKeyFileConcurrentWriter,
		*calculateExtendedStats,
		combinedProbeDebugging,
	)

	responsivenessIsStable := false
	downloadThroughputIsStable := false
	uploadThroughputIsStable := false

	// Test parameters:
	// 1. I: The number of previous instantaneous measurements to consider when generating
	//       the so-called instantaneous moving averages.
	// 2. K: The number of instantaneous moving averages to consider when determining stability.
	// 3: S: The standard deviation cutoff used to determine stability among the K preceding
	//       moving averages of a measurement.
	// See

	throughputI := constants.InstantaneousThroughputMeasurementCount
	probeI := constants.InstantaneousProbeMeasurementCount
	K := constants.InstantaneousMovingAverageStabilityCount
	S := constants.StabilityStandardDeviation

	downloadThroughputStabilizerDebugConfig := debug.NewDebugWithPrefix(debug.Debug, "Download Throughput Stabilizer")
	downloadThroughputStabilizerDebugLevel := debug.Error
	if *debugCliFlag {
		downloadThroughputStabilizerDebugLevel = debug.Debug
	}
	downloadThroughputStabilizer := stabilizer.NewThroughputStabilizer(throughputI, K, S, downloadThroughputStabilizerDebugLevel, downloadThroughputStabilizerDebugConfig)

	uploadThroughputStabilizerDebugConfig := debug.NewDebugWithPrefix(debug.Debug, "Upload Throughput Stabilizer")
	uploadThroughputStabilizerDebugLevel := debug.Error
	if *debugCliFlag {
		uploadThroughputStabilizerDebugLevel = debug.Debug
	}
	uploadThroughputStabilizer := stabilizer.NewThroughputStabilizer(throughputI, K, S, uploadThroughputStabilizerDebugLevel, uploadThroughputStabilizerDebugConfig)

	probeStabilizerDebugConfig := debug.NewDebugWithPrefix(debug.Debug, "Probe Stabilizer")
	probeStabilizerDebugLevel := debug.Error
	if *debugCliFlag {
		probeStabilizerDebugLevel = debug.Debug
	}
	probeStabilizer := stabilizer.NewProbeStabilizer(probeI, K, S, probeStabilizerDebugLevel, probeStabilizerDebugConfig)

	selfRtts := ms.NewInfiniteMathematicalSeries[float64]()
	selfRttsQualityAttenuation := qualityattenuation.NewSimpleQualityAttenuation()
	foreignRtts := ms.NewInfiniteMathematicalSeries[float64]()

	// For later debugging output, record the last throughputs on load-generating connectings
	// and the number of open connections.
	lastUploadThroughputRate := float64(0)
	lastUploadThroughputOpenConnectionCount := int(0)
	lastDownloadThroughputRate := float64(0)
	lastDownloadThroughputOpenConnectionCount := int(0)

	// Every time that there is a new measurement, the possibility exists that the measurements become unstable.
	// This allows us to continue pushing until *everything* is stable at the same time.
timeout:
	for !(responsivenessIsStable && downloadThroughputIsStable && uploadThroughputIsStable) {
		select {

		case downloadThroughputMeasurement := <-downloadThroughputChannel:
			{
				downloadThroughputStabilizer.AddMeasurement(downloadThroughputMeasurement)
				downloadThroughputIsStable = downloadThroughputStabilizer.IsStable()
				if *debugCliFlag {
					fmt.Printf(
						"################# Download is instantaneously %s.\n", utilities.Conditional(downloadThroughputIsStable, "stable", "unstable"))
				}
				downloadThroughputDataLogger.LogRecord(downloadThroughputMeasurement)
				for i := range downloadThroughputMeasurement.GranularThroughputDataPoints {
					datapoint := downloadThroughputMeasurement.GranularThroughputDataPoints[i]
					datapoint.Direction = "Download"
					granularThroughputDataLogger.LogRecord(datapoint)
				}

				lastDownloadThroughputRate = downloadThroughputMeasurement.Throughput
				lastDownloadThroughputOpenConnectionCount = downloadThroughputMeasurement.Connections
			}

		case uploadThroughputMeasurement := <-uploadThroughputChannel:
			{
				uploadThroughputStabilizer.AddMeasurement(uploadThroughputMeasurement)
				uploadThroughputIsStable = uploadThroughputStabilizer.IsStable()
				if *debugCliFlag {
					fmt.Printf(
						"################# Upload is instantaneously %s.\n", utilities.Conditional(uploadThroughputIsStable, "stable", "unstable"))
				}
				uploadThroughputDataLogger.LogRecord(uploadThroughputMeasurement)
				for i := range uploadThroughputMeasurement.GranularThroughputDataPoints {
					datapoint := uploadThroughputMeasurement.GranularThroughputDataPoints[i]
					datapoint.Direction = "Upload"
					granularThroughputDataLogger.LogRecord(datapoint)
				}

				lastUploadThroughputRate = uploadThroughputMeasurement.Throughput
				lastUploadThroughputOpenConnectionCount = uploadThroughputMeasurement.Connections
			}
		case probeMeasurement := <-probeDataPointsChannel:
			{
				probeStabilizer.AddMeasurement(probeMeasurement)

				// Check stabilization immediately -- this could change if we wait. Not sure if the immediacy
				// is *actually* important, but it can't hurt?
				responsivenessIsStable = probeStabilizer.IsStable()

				if *debugCliFlag {
					fmt.Printf(
						"################# Responsiveness is instantaneously %s.\n", utilities.Conditional(responsivenessIsStable, "stable", "unstable"))
				}
				if probeMeasurement.Type == probe.Foreign {
					// There may be more than one round trip accumulated together. If that is the case,
					// we will blow them apart in to three separate measurements and each one will just
					// be 1 / measurement.RoundTripCount of the total length.
					for range utilities.Iota(0, int(probeMeasurement.RoundTripCount)) {
						foreignRtts.AddElement(probeMeasurement.Duration.Seconds() / float64(probeMeasurement.RoundTripCount))

					}
				} else if probeMeasurement.Type == probe.SelfDown || probeMeasurement.Type == probe.SelfUp {
					selfRtts.AddElement(probeMeasurement.Duration.Seconds())
					if *printQualityAttenuation {
						selfRttsQualityAttenuation.AddSample(probeMeasurement.Duration.Seconds())
					}
				}

				if probeMeasurement.Type == probe.Foreign {
					foreignProbeDataLogger.LogRecord(probeMeasurement)
				} else if probeMeasurement.Type == probe.SelfDown || probeMeasurement.Type == probe.SelfUp {
					selfProbeDataLogger.LogRecord(probeMeasurement)
				}
			}
		case <-timeoutChannel:
			{
				break timeout
			}
		}
	}

	// TODO: Reset timeout to RPM timeout stat?

	// Did the test run to stability?
	testRanToStability := (downloadThroughputIsStable && uploadThroughputIsStable && responsivenessIsStable)

	if *debugCliFlag {
		fmt.Printf("Stopping all the load generating data generators (stability: %s).\n", utilities.Conditional(testRanToStability, "success", "failure"))
	}

	/* At this point there are
	1. Load generators running
	-- uploadLoadGeneratorOperatorCtx
	-- downloadLoadGeneratorOperatorCtx
	2. Network connections opened by those load generators:
	-- lgNetworkActivityCtx
	3. Probes
	-- proberCtx
	*/

	// First, stop the load generator and the probe operators (but *not* the network activity)
	proberOperatorCtxCancel()
	downloadLoadGeneratorOperatorCtxCancel()
	uploadLoadGeneratorOperatorCtxCancel()

	// Second, calculate the extended stats (if the user requested)

	extendedStats := extendedstats.AggregateExtendedStats{}
	if *calculateExtendedStats {
		if extendedstats.ExtendedStatsAvailable() {
			func() {
				// Put inside an IIFE so that we can use a defer!
				downloadLoadGeneratingConnectionCollection.Lock.Lock()
				defer downloadLoadGeneratingConnectionCollection.Lock.Unlock()

				// Note: We do not trace upload connections!
				for i := 0; i < downloadLoadGeneratingConnectionCollection.Len(); i++ {
					// Assume that extended statistics are available -- the check was done explicitly at
					// program startup if the calculateExtendedStats flag was set by the user on the command line.
					currentLgc, _ := downloadLoadGeneratingConnectionCollection.Get(i)
					if err := extendedStats.IncorporateConnectionStats((*currentLgc).Stats().ConnInfo.Conn); err != nil {
						fmt.Fprintf(
							os.Stderr,
							"Warning: Could not add extended stats for the connection: %v\n",
							err,
						)
					}
				}
			}()
		} else {
			// TODO: Should we just log here?
			panic("Extended stats are not available but the user requested their calculation.")
		}
	}

	// Third, stop the network connections opened by the load generators and probers.
	networkActivityCtxCancel()

	// Finally, stop the world.
	operatingCtxCancel()

	// Calculate the RPM

	// First, let's do a double-sided trim of the top/bottom 10% of our measurements.
	selfRttsTotalCount := selfRtts.Len()
	foreignRttsTotalCount := foreignRtts.Len()

	selfRttsTrimmed := selfRtts.DoubleSidedTrim(10)
	foreignRttsTrimmed := foreignRtts.DoubleSidedTrim(10)

	selfRttsTrimmedCount := selfRttsTrimmed.Len()
	foreignRttsTrimmedCount := foreignRttsTrimmed.Len()

	// Then, let's take the mean of those ...
	selfProbeRoundTripTimeMean := selfRttsTrimmed.CalculateAverage()
	foreignProbeRoundTripTimeMean := foreignRttsTrimmed.CalculateAverage()

	// Second, let's do the P90 calculations.
	selfProbeRoundTripTimeP90 := selfRtts.Percentile(90)
	foreignProbeRoundTripTimeP90 := foreignRtts.Percentile(90)

	// Note: The specification indicates that we want to calculate the foreign probes as such:
	// 1/3*tcp_foreign + 1/3*tls_foreign + 1/3*http_foreign
	// where tcp_foreign, tls_foreign, http_foreign are the P90 RTTs for the connection
	// of the tcp, tls and http connections, respectively. However, we cannot break out
	// the individual RTTs so we assume that they are roughly equal.

	// This is 60 because we measure in seconds not ms
	p90Rpm := 60.0 / (float64(selfProbeRoundTripTimeP90+foreignProbeRoundTripTimeP90) / 2.0)
	meanRpm := 60.0 / (float64(selfProbeRoundTripTimeMean+foreignProbeRoundTripTimeMean) / 2.0)

	if *debugCliFlag {
		fmt.Printf(
			`Total Self Probes:            %d
Total Foreign Probes:         %d
Trimmed Self Probes Count:    %d
Trimmed Foreign Probes Count: %d
P90 Self RTT:                 %f
P90 Foreign RTT:              %f
Trimmed Mean Self RTT:        %f
Trimmed Mean Foreign RTT:     %f
`,
			selfRttsTotalCount,
			foreignRttsTotalCount,
			selfRttsTrimmedCount,
			foreignRttsTrimmedCount,
			selfProbeRoundTripTimeP90,
			foreignProbeRoundTripTimeP90,
			selfProbeRoundTripTimeMean,
			foreignProbeRoundTripTimeMean,
		)
	}

	if *printQualityAttenuation {
		fmt.Println("Quality Attenuation Statistics:")
		fmt.Printf(
			`Number of losses: %d
Number of samples: %d
Loss: %f
Min: %.6f
Max: %.6f
Mean: %.6f 
Variance: %.6f
Standard Deviation: %.6f
PDV(90): %.6f
PDV(99): %.6f
P(90): %.6f
P(99): %.6f
`, selfRttsQualityAttenuation.GetNumberOfLosses(),
			selfRttsQualityAttenuation.GetNumberOfSamples(),
			selfRttsQualityAttenuation.GetLossPercentage(),
			selfRttsQualityAttenuation.GetMinimum(),
			selfRttsQualityAttenuation.GetMaximum(),
			selfRttsQualityAttenuation.GetAverage(),
			selfRttsQualityAttenuation.GetVariance(),
			selfRttsQualityAttenuation.GetStandardDeviation(),
			selfRttsQualityAttenuation.GetPDV(90),
			selfRttsQualityAttenuation.GetPDV(99),
			selfRttsQualityAttenuation.GetPercentile(90),
			selfRttsQualityAttenuation.GetPercentile(99))
	}

	if !testRanToStability {
		fmt.Printf("Test did not run to stability, these results are estimates:\n")
	}

	fmt.Printf("RPM: %5.0f (P90)\n", p90Rpm)
	fmt.Printf("RPM: %5.0f (Double-Sided 10%% Trimmed Mean)\n", meanRpm)

	fmt.Printf(
		"Download: %7.3f Mbps (%7.3f MBps), using %d parallel connections.\n",
		utilities.ToMbps(lastDownloadThroughputRate),
		utilities.ToMBps(lastDownloadThroughputRate),
		lastDownloadThroughputOpenConnectionCount,
	)
	fmt.Printf(
		"Upload:   %7.3f Mbps (%7.3f MBps), using %d parallel connections.\n",
		utilities.ToMbps(lastUploadThroughputRate),
		utilities.ToMBps(lastUploadThroughputRate),
		lastUploadThroughputOpenConnectionCount,
	)

	if *calculateExtendedStats {
		fmt.Println(extendedStats.Repr())
	}

	selfProbeDataLogger.Export()
	if *debugCliFlag {
		fmt.Printf("Closing the self data logger.\n")
	}
	selfProbeDataLogger.Close()

	foreignProbeDataLogger.Export()
	if *debugCliFlag {
		fmt.Printf("Closing the foreign data logger.\n")
	}
	foreignProbeDataLogger.Close()

	downloadThroughputDataLogger.Export()
	if *debugCliFlag {
		fmt.Printf("Closing the download throughput data logger.\n")
	}
	downloadThroughputDataLogger.Close()

	uploadThroughputDataLogger.Export()
	if *debugCliFlag {
		fmt.Printf("Closing the upload throughput data logger.\n")
	}
	uploadThroughputDataLogger.Close()

	granularThroughputDataLogger.Export()
	if *debugCliFlag {
		fmt.Printf("Closing the granular throughput data logger.\n")
	}
	granularThroughputDataLogger.Close()

	if *debugCliFlag {
		fmt.Printf("In debugging mode, we will cool down.\n")
		time.Sleep(constants.CooldownPeriod)
		fmt.Printf("Done cooling down.\n")
	}

	if len(*prometheusStatsFilename) > 0 {
		var testStable int
		if testRanToStability {
			testStable = 1
		}
		var buffer bytes.Buffer
		buffer.WriteString(fmt.Sprintf("networkquality_test_stable %d\n", testStable))
		buffer.WriteString(fmt.Sprintf("networkquality_rpm_value %d\n", int64(p90Rpm)))
		buffer.WriteString(fmt.Sprintf("networkquality_trimmed_rpm_value %d\n", int64(meanRpm))) //utilities.ToMbps(lastDownloadThroughputRate),

		buffer.WriteString(fmt.Sprintf("networkquality_download_bits_per_second %d\n", int64(lastDownloadThroughputRate)))
		buffer.WriteString(fmt.Sprintf("networkquality_download_connections %d\n", int64(lastDownloadThroughputOpenConnectionCount)))
		buffer.WriteString(fmt.Sprintf("networkquality_upload_bits_per_second %d\n", int64(lastUploadThroughputRate)))
		buffer.WriteString(fmt.Sprintf("networkquality_upload_connections %d\n", lastUploadThroughputOpenConnectionCount))

		if err := os.WriteFile(*prometheusStatsFilename, buffer.Bytes(), 0644); err != nil {
			fmt.Printf("could not write %s: %s", *prometheusStatsFilename, err)
			os.Exit(1)
		}
	}
}
