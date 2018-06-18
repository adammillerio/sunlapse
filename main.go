// Package main of sunlapse provides an application for taking repeated images
// from a webcam during daytime hours and creating a timelapse video of them.
package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"os/exec"
	"time"

	"github.com/kelseyhightower/envconfig"
	"github.com/kelvins/sunrisesunset"
	log "github.com/sirupsen/logrus"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	drive "google.golang.org/api/drive/v3"
)

// Type config represents configuration of the sunlapse application, with struct
// tags corresponding to the envconfig package which processes it.
type config struct {
	Period          int     `default:"30"`
	Timeout         int     `default:"5"`
	LogLevel        string  `default:"info"`
	DriveTokenFile  string  `default:"drive_token.json"`
	DriveSecretFile string  `default:"drive_client_secret.json"`
	Endpoint        string  `required:"true"`
	Latitude        float64 `required:"true"`
	Longitude       float64 `required:"true"`
	Offset          float64 `required:"true"`
}

// Package level config, http.Client, Drive service, and if it is being used
var (
	conf         config
	client       http.Client
	localMode    bool
	driveService *drive.Service
)

func init() {
	// Parse environment variables
	err := envconfig.Process("sunlapse", &conf)
	if err != nil {
		log.Fatalf("Error parsing environment variables: %s", err)
	}

	// Logging options
	log.SetFormatter(&log.JSONFormatter{})
	log.SetOutput(os.Stdout)

	switch conf.LogLevel {
	case "info":
		log.SetLevel(log.InfoLevel)
	case "debug":
		log.SetLevel(log.DebugLevel)
	case "warn":
		log.SetLevel(log.WarnLevel)
	case "error":
		log.SetLevel(log.ErrorLevel)
	default:
		log.SetLevel(log.InfoLevel)
	}

	// Authentication with Google Drive
	driveService, err = getDriveService()
	if err != nil {
		log.Errorf("Error authenticating with Google Drive: %s", err)
		log.Errorf("Running in local-only mode")
		localMode = true
	}

	// HTTP client options
	client.Timeout = time.Second * time.Duration(conf.Timeout)

	// Create working directories if they do not exist
	dirs := []string{
		"./tmp/images",
		"./tmp/videos",
		"./tmp/archives",
	}

	for _, dir := range dirs {
		err = createDirectory(dir)
		if err != nil {
			log.Fatal(err)
		}
	}
}

func main() {
	// Channel and goroutine for "summarizing" the day
	summaryChannel := make(chan time.Time)
	go func() {
		for summaryTime := range summaryChannel {
			// Create the timelapse video
			log.Info("Creating video")

			err := createVideo(summaryTime)
			if err != nil {
				log.Errorf("Error creating video: %s", err)
			} else {
				// If able to make video, make archive
				log.Info("Creating image archive")

				err = archiveImages(summaryTime)
				if err != nil {
					log.Errorf("Error creating image archive: %s", err)
				} else {
					// If able to make archive, delete images
					log.Info("Deleting images")

					err = deleteImages(summaryTime)
					if err != nil {
						log.Errorf("Error deleting image archive: %s", err)
					}
				}
			}
		}
	}()

	// Ticker and routine for taking images and checking if it is daytime
	ticker := time.NewTicker(time.Second * time.Duration(conf.Period))
	go func() {
		// Initially compute daytime
		sunrise, sunset, err := getSunriseSunset(conf.Latitude, conf.Longitude,
			conf.Offset, time.Now())
		if err != nil {
			log.Fatalf("Error calculating Sunrise/Sunset: %s", err)
		}

		// Get current time and directory name
		curTime := time.Now()
		dir := fmt.Sprintf("./tmp/images/%s", curTime.Format("2006-01-02"))

		// Check if it is daytime at time of run
		isDaytime := inTimeSpan(sunrise, sunset, curTime)
		if isDaytime {
			// If it is,  make the day's directory for images
			log.Debug("It is currently daytime")

			err := createDirectory(dir)
			if err != nil {
				log.Fatal(err)
			}
		} else {
			// Otherwise, calculate times for tomorrow's date
			log.Debug("It is not currently daytime")

			sunrise, sunset, err = getSunriseSunset(conf.Latitude, conf.Longitude,
				conf.Offset, time.Now().AddDate(0, 0, 1))
			if err != nil {
				log.Fatalf("Error calculating tomorrow's Sunrise/Sunset: %s", err)
			}
		}

		log.Infof("Sunrise: %s", sunrise.Format(time.RFC3339))
		log.Infof("Sunset: %s", sunset.Format(time.RFC3339))

		// Set index for image name
		var imageIndex int = 1

		// Range infinitely over the ticker defined in the main loop, which will
		// tick and return a time every SUNLAPSE_PERIOD
		for curTime := range ticker.C {
			log.Debugf("Current sunrise: %s", sunrise.Format(time.RFC3339))
			log.Debugf("Current sunset: %s", sunset.Format(time.RFC3339))
			log.Debugf("Current time: %s", curTime.Format(time.RFC3339))

			if inTimeSpan(sunrise, sunset, curTime) {
				// If it's daytime, enter daytime loop
				if !isDaytime {
					// If this is the first instance of daytime, set the boolean to true,
					// reset the index, and create today's image directory
					log.Info("It has now passed sunrise")
					isDaytime = true
					imageIndex = 1
					dir = fmt.Sprintf("./tmp/images/%s", curTime.Format("2006-01-02"))

					err := createDirectory(dir)
					if err != nil {
						log.Fatal(err)
					}
				}

				// Grab an image
				log.Info("Getting image")

				image, err := getImage(conf.Endpoint)
				if err != nil {
					// If you can't get the image, error and wait for next loop
					log.Errorf("Error while getting image: %s", err)
				} else {
					// Otherwise, save the image
					log.Info("Writing image")

					err = ioutil.WriteFile(fmt.Sprintf("%s/image_%05d.jpg",
						dir, imageIndex), image, 0755)
					if err != nil {
						// If you can't save the image, don't increment the counter
						log.Errorf("Error while writing image: %s", err)
					} else {
						// Otherwise, increment the image counter
						imageIndex += 1
					}
				}
			} else {
				// Otherwise, enter the non-daytime loop
				if isDaytime {
					// If this is the first instance of it not being daytime, signal the
					// summary channel with current time and set boolean to false
					log.Info("It has now passed sunset, beginning summary routine")
					summaryChannel <- curTime
					isDaytime = false

					// Calculate the new sunrise and sunset thresholds for tomorrow
					log.Info("Calculating tomorrow's Sunrise/Sunset")

					sunrise, sunset, err = getSunriseSunset(conf.Latitude, conf.Longitude,
						conf.Offset, time.Now().AddDate(0, 0, 1))
					if err != nil {
						log.Fatalf("Error calculating tomorrow's Sunrise/Sunset: %s", err)
					}
				} else {
					// If it's not the first instance, just sleep for this loop
					log.Info("Not currently daytime, sleeping...")
				}
			}
		}
	}()

	// Indefinitely sleep the main goroutine
	for {
		time.Sleep(time.Second * 30)
	}
}

// createDirectory takes a directory path and creates the directory if it does
// not already exist.
// It returns an error if encountered during creation.
func createDirectory(dir string) error {
	cLog := log.WithFields(log.Fields{
		dir: dir,
	})

	if _, err := os.Stat(dir); os.IsNotExist(err) {
		// If directory does not already exist, create it
		cLog.Infof("Creating %s directory", dir)

		err = os.MkdirAll(dir, 0755)
		if err != nil {
			cLog.Errorf("Unable to create %s directory: %s", dir, err)
		}

		return nil
	}

	return nil
}

// deleteImages takes a time and deletes the images directory and all images.
// It returns an error if encountered during deletion.
func deleteImages(date time.Time) error {
	// Form directory name: ./tmp/images/2006-01-02
	dir := fmt.Sprintf("./tmp/images/%s", date.Format("2006-01-02"))

	cLog := log.WithFields(log.Fields{
		"date": date,
		"dir":  dir,
	})

	// Delete all images and directory
	err := os.RemoveAll(dir)
	if err != nil {
		cLog.Error(err)
		return err
	}

	return nil
}

// archiveImages takes a time and creates a tar archive with all images.
// It returns an error if encountered during archival.
func archiveImages(date time.Time) error {
	// Form directory name: ./tmp/images/2006-01-02
	dir := fmt.Sprintf("./tmp/images/%s", date.Format("2006-01-02"))

	cLog := log.WithFields(log.Fields{
		"date": date,
		"dir":  dir,
	})

	// Create the archive file
	archiveFile, err := os.Create(fmt.Sprintf("./tmp/archives/%s.tar.gz",
		date.Format("2006-01-02")))
	if err != nil {
		cLog.Error(err)
		return err
	}
	defer archiveFile.Close()

	// Create a gzip writer
	gzWriter := gzip.NewWriter(archiveFile)
	defer gzWriter.Close()

	// Create a tar writer that writes into the gzip writer
	archive := tar.NewWriter(gzWriter)
	defer archive.Close()

	// Read the directory matching the date
	files, err := ioutil.ReadDir(dir)
	if err != nil {
		cLog.Error(err)
		return err
	}

	// Add each file to the archive
	for _, path := range files {
		// Form the full path of the file from the stat
		fullPath := fmt.Sprintf("%s/%s", dir, path.Name())

		// Open the file
		file, err := os.Open(fullPath)
		if err != nil {
			cLog.Error(err)
			continue
		}

		// Build the tar header from the stat
		header, err := tar.FileInfoHeader(path, fullPath)
		if err != nil {
			cLog.Error(err)
			file.Close()
			continue
		}

		// Write the header
		err = archive.WriteHeader(header)
		if err != nil {
			cLog.Error(err)
			file.Close()
			continue
		}

		// Copy the file into the tar writer
		_, err = io.Copy(archive, file)
		if err != nil {
			cLog.Error(err)
			file.Close()
			continue
		}

		file.Close()
	}

	return nil
}

// createVideo takes a time and creates a timelapse video from the images.
// It returns an error if encountered during creation.
func createVideo(date time.Time) error {
	// Form directory name: ./tmp/images/2006-01-02
	dir := fmt.Sprintf("./tmp/images/%s", date.Format("2006-01-02"))

	// Run ffmpeg for the time provided
	err := runCommand("ffmpeg", "-y", "-f", "image2", "-i",
		fmt.Sprintf("%s/image_%%05d.jpg", dir), "-r", "30", "-q:v",
		"2", "-pix_fmt", "yuvj420p", "-vcodec", "libx264",
		fmt.Sprintf("./tmp/videos/%s.mp4", date.Format("2006-01-02")))
	if err != nil {
		log.Error(err)
		return err
	}

	return nil
}

// runCommand takes a command and any arguments and runs it.
// It returns an error if encountered during execution.
func runCommand(command string, args ...string) error {
	cLog := log.WithFields(log.Fields{
		"command": command,
		"args":    args,
	})

	// Create the cmd
	process := exec.Command(command, args...)

	// Create and associate buffers for stdout and stderr
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	process.Stdout = stdout
	process.Stderr = stderr

	// Run the cmd
	err := process.Run()
	if err != nil {
		cLog.Error(string(stderr.Bytes()))
		cLog.Debug(string(stdout.Bytes()))
		return err
	}

	cLog.Debug(string(stderr.Bytes()))
	cLog.Debug(string(stdout.Bytes()))
	return nil
}

// getImage retrieves an image from a provided url.
// It returns a byte slice with the image contents and any errors encountered.
func getImage(url string) ([]byte, error) {
	cLog := log.WithFields(log.Fields{
		"url": url,
	})

	// Byte slice that will eventually hold the image contents
	var image []byte

	// Create an HTTP request based on the provided URL endpoint, returning an
	// error if the request cannot be created.
	request, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		cLog.Error(err)
		return image, err
	}

	// Make the HTTP request with the shared http Client, returning an error if
	// the request fails or times out.
	response, err := client.Do(request)
	if err != nil {
		cLog.Error(err)
		return image, err
	}

	// Check if the status code is OK (200) and return an error if it is not.
	if response.StatusCode != http.StatusOK {
		cLog.Error(err)
		return image, errors.New("non-200 status code received")
	}

	// Parse the response body into a byte slice, returning an error if unable to
	// parse.
	image, err = ioutil.ReadAll(response.Body)
	if err != nil {
		cLog.Error(err)
		return image, err
	}

	// Return the byte slice.
	return image, nil
}

// inTimeSpan takes a start and end time as well as a time and checks if the
// time is in range.
// It returns a boolean representing whether or not it is in range.
func inTimeSpan(start, end, check time.Time) bool {
	return check.After(start) && check.Before(end)
}

// getSunriseSunset takes a latitude, longitude, UTC offset, and a time, and
// calculates the sunrise and sunset.
// It returns time objects representing sunrise and sunset as well as any errors
func getSunriseSunset(lat float64, long float64, offset float64,
	date time.Time) (time.Time, time.Time, error) {
	cLog := log.WithFields(log.Fields{
		"latitude":  lat,
		"longitude": long,
		"offset":    offset,
		"date":      time.Now(),
	})

	// Create the parameters object using provided values
	sunCalc := sunrisesunset.Parameters{
		Latitude:  lat,
		Longitude: long,
		UtcOffset: offset,
		Date:      date,
	}

	// Calculate the sunrise and sunset
	sunrise, sunset, err := sunCalc.GetSunriseSunset()
	if err != nil {
		cLog.Error(err)
		return sunrise, sunset, err
	}

	cLog.Debugf("Before correction - Sunrise: %s, Sunset: %s",
		sunrise.Format(time.RFC3339), sunset.Format(time.RFC3339))

	// sunrisesunset returns time objects with only the hour, minute, and second
	// values provided, leaving all others in their default state.
	// Because of this, we must create a new time object that also includes the
	// correct values corresponding to the date provided.
	// e.g. 1001-01-01 15:04:03 becomes 2018-06-10 15:04:03
	sunset = time.Date(date.Year(), date.Month(), date.Day(),
		sunset.Hour(), sunset.Minute(), sunset.Second(), 0, time.Local)
	sunrise = time.Date(date.Year(), date.Month(), date.Day(),
		sunrise.Hour(), sunrise.Minute(), sunrise.Second(), 0, time.Local)

	cLog.Debugf("After correction - Sunrise: %s, Sunset: %s",
		sunrise.Format(time.RFC3339), sunset.Format(time.RFC3339))

	return sunrise, sunset, nil
}

// tokenFromFile loads a provided file path, and unmarshals it into an oauth
// token.
// It returns an *oauth2.Token and any errors encountered.
func tokenFromFile(path string) (*oauth2.Token, error) {
	cLog := log.WithFields(log.Fields{
		"path": path,
	})

	// Load the token file
	file, err := os.Open(path)
	defer file.Close()
	if err != nil {
		cLog.Errorf("Error loading OAuth token from file: %s", err)
		return nil, err
	}

	// Create the token object
	token := &oauth2.Token{}

	// Unmarshal the JSON string into the oauth token
	err = json.NewDecoder(file).Decode(token)
	if err != nil {
		cLog.Errorf("Error Decoding OAuth token from file: %s", err)
		return nil, err
	}

	// Return the token
	return token, nil
}

// saveToken takes a path and an oauth2.Token and saves it to the path specified
// It returns any errors encountered while saving.
func saveToken(path string, token *oauth2.Token) error {
	cLog := log.WithFields(log.Fields{
		"path": path,
	})

	// Open the file handle
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0600)
	defer file.Close()
	if err != nil {
		cLog.Errorf("Error opening OAuth token file for saving: %s", err)
		return err
	}

	// Marshal the token and save it into the file
	err = json.NewEncoder(file).Encode(token)
	if err != nil {
		cLog.Errorf("Error savin")
		return err
	}

	return nil
}

// tokenFromWeb takes an oauth2.Config prompts the user to manually generate
// an authentication token in their browser.
// It returns an *oauth2.Token and any errors encountered.
func tokenFromWeb(config *oauth2.Config) (*oauth2.Token, error) {
	// Generate the URL for manually authenticating via OAuth
	authURL := config.AuthCodeURL("state-token", oauth2.AccessTypeOffline)

	// Print a prompt indicating to go to the URL and authenticate
	fmt.Printf("Go to the following link in your browser then type the "+
		"authorization code: \n%v\n", authURL)

	// Scan for the user input of the authentication code
	var authCode string
	_, err := fmt.Scan(&authCode)
	if err != nil {
		log.Errorf("Error loading input OAuth token: %s", err)
		return nil, err
	}

	// Exchange authentication information using the code
	token, err := config.Exchange(oauth2.NoContext, authCode)
	if err != nil {
		log.Errorf("Error authenticating using input Oauth token: %s", err)
		return nil, err
	}

	// Return the authentication token
	return token, nil
}

// getOauthClient takes a Google OAuth configuration and returns a generated
// authenticated http Client.
// It returns an authenticated http.Client and any errors encountered.
func getOauthClient(config *oauth2.Config) (*http.Client, error) {
	// Set the token file to the specified config value
	tokenFile := conf.DriveTokenFile

	// Attempt to load an existing token from the file
	token, err := tokenFromFile(tokenFile)
	if err != nil {
		// If not found, attempt to generate a new one from the web
		token, err = tokenFromWeb(config)
		if err != nil {
			// If unable to generate, error
			log.Errorf("Error generating oauth token from web: %s", err)
			return nil, err
		}

		// Otherwise, save the generated token for later use
		saveToken(tokenFile, token)
	}

	// Return an oauth client authenticated with the token
	return config.Client(context.Background(), token), nil
}

// getDriveService creates an authenticated client for interacting with the
// Google Drive API.
// It returns a *drive.Service and any errors encountered.
func getDriveService() (*drive.Service, error) {
	// Load the client secret
	secret, err := ioutil.ReadFile(conf.DriveSecretFile)
	if err != nil {
		log.Errorf("Error encountered loading Google OAuth secret: %s", err)
		return nil, err
	}

	// Generate a Google OAuth configuration from the JSON byte slice
	// Uses drive.DriveMetadataReadonlyScope
	config, err := google.ConfigFromJSON(secret, drive.DriveFileScope)
	if err != nil {
		log.Errorf("Error encountered creating Google Oauth config: %s", err)
		return nil, err
	}

	// Get an OAuth authenticated http.Client
	client, err := getOauthClient(config)
	if err != nil {
		log.Errorf("Error encountered creating Oauth client: %s", err)
		return nil, err
	}

	// Create the Drive Service using the authenticated http.Client
	service, err := drive.New(client)
	if err != nil {
		log.Errorf("Error encountered creating Drive Service: %s", err)
		return nil, err
	}

	// Return the Drive Service
	return service, nil
}

// createDriveDirectory takes a directory name and creates the corresponding
// directory in Google Drive.
// It returns any errors encountered during creation.
func createDriveDirectory(dir string) error {
	cLog := log.WithFields(log.Fields{
		"dir": dir,
	})

	// Create the directory File
	// A directory is just a File with a special MimeType in Drive
	directory := &drive.File{
		Name:     dir,
		MimeType: "application/vnd.google-apps.folder",
	}

	// Tell the drive service to create the directory
	_, err := driveService.Files.Create(directory).Do()
	if err != nil {
		cLog.Errorf("Error creating drive directory: %s", err)
		return err
	}

	return nil
}
