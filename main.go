package main

import (
	"context"
	"flag"
	"fmt"
	"io/ioutil"
	"os"
	"os/signal"
	"os/user"
	"path/filepath"
	"syscall"
	"time"

	"github.com/jessfraz/tripitcalb0t/tripit"
	"github.com/jessfraz/tripitcalb0t/version"
	"github.com/sirupsen/logrus"
	"golang.org/x/oauth2/google"
	calendar "google.golang.org/api/calendar/v3"
)

const (
	// BANNER is what is printed for help/info output.
	BANNER = ` _        _       _ _            _ _      ___  _
| |_ _ __(_)_ __ (_) |_ ___ __ _| | |__  / _ \| |_
| __| '__| | '_ \| | __/ __/ _` + "`" + ` | | '_ \| | | | __|
| |_| |  | | |_) | | || (_| (_| | | |_) | |_| | |_
 \__|_|  |_| .__/|_|\__\___\__,_|_|_.__/ \___/ \__|
           |_|

 Bot to automatically create Google Calendar events from TripIt flight data.
 Version: %s
 Build: %s

`
)

var (
	googleCalendarKeyfile string
	calendarName          string
	credsDir              string

	tripitUsername string
	tripitToken    string

	interval string
	once     bool

	debug bool
	vrsn  bool
)

func init() {
	// Get home directory.
	home, err := getHome()
	if err != nil {
		logrus.Fatal(err)
	}
	credsDir = filepath.Join(home, ".tripitcalb0t")

	// parse flags
	flag.StringVar(&googleCalendarKeyfile, "google-keyfile", filepath.Join(credsDir, "google.json"), "Path to Google Calendar keyfile")
	flag.StringVar(&calendarName, "calendar", os.Getenv("GOOGLE_CALENDAR_ID"), "Calendar name to add events to (or env var GOOGLE_CALENDAR_ID)")

	flag.StringVar(&tripitUsername, "tripit-username", os.Getenv("TRIPIT_USERNAME"), "TripIt Username for authentication (or env var TRIPIT_USERNAME)")
	flag.StringVar(&tripitToken, "tripit-token", os.Getenv("TRIPIT_TOKEN"), "TripIt Token for authentication (or env var TRIPIT_TOKEN)")

	flag.StringVar(&interval, "interval", "1m", "update interval (ex. 5ms, 10s, 1m, 3h)")
	flag.BoolVar(&once, "once", false, "run once and exit, do not run as a daemon")

	flag.BoolVar(&vrsn, "version", false, "print version and exit")
	flag.BoolVar(&vrsn, "v", false, "print version and exit (shorthand)")
	flag.BoolVar(&debug, "d", false, "run in debug mode")

	flag.Usage = func() {
		fmt.Fprint(os.Stderr, fmt.Sprintf(BANNER, version.VERSION, version.GITCOMMIT))
		flag.PrintDefaults()
	}

	flag.Parse()

	if vrsn {
		fmt.Printf("tripitcalb0t version %s, build %s", version.VERSION, version.GITCOMMIT)
		os.Exit(0)
	}

	// set log level
	if debug {
		logrus.SetLevel(logrus.DebugLevel)
	}

	if tripitUsername == "" {
		usageAndExit("tripit username cannot be empty", 1)
	}

	if tripitToken == "" {
		usageAndExit("tripit token cannot be empty", 1)
	}

	if _, err := os.Stat(googleCalendarKeyfile); os.IsNotExist(err) {
		usageAndExit(fmt.Sprintf("Google Calendar keyfile %q does not exist", googleCalendarKeyfile), 1)
	}
}

func main() {
	var ticker *time.Ticker

	// On ^C, or SIGTERM handle exit.
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)
	signal.Notify(c, syscall.SIGTERM)
	go func() {
		for sig := range c {
			ticker.Stop()
			logrus.Infof("Received %s, exiting.", sig.String())
			os.Exit(0)
		}
	}()

	// Parse the duration.
	dur, err := time.ParseDuration(interval)
	if err != nil {
		logrus.Fatalf("parsing %s as duration failed: %v", interval, err)
	}
	ticker = time.NewTicker(dur)

	// Create the TripIt API client.
	tripitClient := tripit.New(tripitUsername, tripitToken)

	// Create the Google calendar API client.
	gcalData, err := ioutil.ReadFile(googleCalendarKeyfile)
	if err != nil {
		logrus.Fatalf("reading file %s failed: %v", googleCalendarKeyfile, err)
	}
	gcalTokenSource, err := google.JWTConfigFromJSON(gcalData, calendar.CalendarReadonlyScope)
	if err != nil {
		logrus.Fatalf("creating google calendar token source from file %s failed: %v", googleCalendarKeyfile, err)
	}

	// Create our context.
	ctx := context.Background()

	// Create the Google calendar client.
	gcalClient, err := calendar.New(gcalTokenSource.Client(ctx))
	if err != nil {
		logrus.Fatalf("creating google calendar client failed: %v", err)
	}

	// If the user passed the once flag, just do the run once and exit.
	if once {
		run(tripitClient, gcalClient)
		logrus.Info("Updated TripIt calendar entries")
		os.Exit(0)
	}

	logrus.Infof("Starting bot to update TripIt calendar entries every %s", interval)
	for range ticker.C {
		run(tripitClient, gcalClient)
	}
}

func run(tripitClient *tripit.Client, gcalClient *calendar.Service) {
	// Get a list of calendars.
	calendars, err := gcalClient.CalendarList.List().Do()
	if err != nil {
		logrus.Fatalf("getting calendars from google calendar failed: %v", err)
	}
	for _, cal := range calendars.Items {
		logrus.Infof("calendar: %#v", *cal)
	}

	// Get a list of events.
	t := time.Now().Format(time.RFC3339)
	events, err := gcalClient.Events.List(calendarName).ShowDeleted(false).SingleEvents(true).TimeMin(t).MaxResults(10).OrderBy("startTime").Do()
	if err != nil {
		logrus.Fatalf("getting events from google calendar %s failed: %v", calendarName, err)
	}
	for _, e := range events.Items {
		logrus.Infof("event: %#v", *e)
	}

	// Get a list of trips.
	resp, err := tripitClient.ListTrips(
		tripit.Filter{
			Type:  tripit.FilterPast,
			Value: "true",
		},
		tripit.Filter{
			Type:  tripit.FilterIncludeObjects,
			Value: "true",
		})
	if err != nil {
		logrus.Fatal(err)
	}

	// Iterate over our flights and create/update calendar entries in Google calendar.
	for _, flight := range resp.Flights {
		// Create the events for the flight.
		events, err := flight.GetFlightSegmentsAsEvents()
		if err != nil {
			// Warn on error and continue iterating through the flights.
			logrus.Warn(err)
			continue
		}

		logrus.Infof("events: %#v", events)

		// Create / Update a Google Calendar entry for each event.
		// TODO(jessfraz): do this.
	}
}

func usageAndExit(message string, exitCode int) {
	if message != "" {
		fmt.Fprintf(os.Stderr, message)
		fmt.Fprintf(os.Stderr, "\n\n")
	}
	flag.Usage()
	fmt.Fprintf(os.Stderr, "\n")
	os.Exit(exitCode)
}

func getHome() (string, error) {
	home := os.Getenv(homeKey)
	if home != "" {
		return home, nil
	}

	u, err := user.Current()
	if err != nil {
		return "", err
	}
	return u.HomeDir, nil
}
