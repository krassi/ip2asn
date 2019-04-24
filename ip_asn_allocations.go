package main

import (
	"bufio"
	"bytes"
	"database/sql"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"regexp"
	"strconv"

	"github.com/go-sql-driver/mysql"
	_ "github.com/go-sql-driver/mysql"
)

type FileHeader struct {
	version   string // format version number of this file, currently 2.3;
	registry  string // as for records and filename (see below);
	serial    uint64 // serial number of this file (within the creating RIR series);
	records   uint64 // number of records in file, excluding blank lines, summary lines, the version line and comments;
	startdate string // start date of time period, in yyyymmdd format;
	enddate   string // end date of period in yyyymmdd format;
	UTCoffset int64  // offset from UTC (+/- hours) of local RIR producing file.
	asnCount  uint64 // sum of the number of record lines of this type in the file.
	ipv4Count uint64 // sum of the number of record lines of this type in the file.
	ipv6Count uint64 // sum of the number of recoip2asnrd lines of this type in the file.
}

var f_debug, f_force, f_invalid_hdr_ok *bool
var f_verbose *uint
var f_inputFileName, f_URL, f_source *string

func parseVersionLine(hdr *FileHeader, line string) bool {

	re := regexp.MustCompile(`^([1-9.])+\|(afrinic|apnic|arin|lacnic|ripencc)\|([0-9]+)\|(\d+)\|(\d+)\|(\d+)\|(.*)`)
	matches := re.FindStringSubmatch(line)
	if matches == nil {
		if *f_invalid_hdr_ok != true {
			log.Fatal("Invalid file header and -invalid-header-ok not specified")
		}
		verbosePrint(2, "Warning: date file header missing or corrupt; ignoring due to -invalid-header-ok=true\n")
		return false
	}

	// Initialize header structure
	hdr.version = matches[1]
	hdr.registry = matches[2]
	hdr.serial, _ = strconv.ParseUint(matches[3], 10, 32)
	hdr.records, _ = strconv.ParseUint(matches[4], 10, 32)
	hdr.startdate = matches[5]
	hdr.enddate = matches[6]
	hdr.UTCoffset, _ = strconv.ParseInt(matches[7], 10, 32)
	hdr.UTCoffset /= 100 // TODO: Fix time handling

	// Data corrections
	if hdr.startdate == "00000000" {
		hdr.startdate = "19700101"
	}

	verbosePrint(3, fmt.Sprintf("VERSION LINE PARSED OK: HEADER FIELDS: %s::%s::%d::%d::%s::%s::%d\n", hdr.version,
		hdr.registry, hdr.serial, hdr.records, hdr.startdate, hdr.enddate, hdr.UTCoffset))
	return true
}

func parseSummaryLine(hdr *FileHeader, line string) {
	verbosePrint(3, fmt.Sprintf("HEADER LINE: %s\n", line))
	re := regexp.MustCompile(`^(afrinic|apnic|arin|lacnic|ripencc)\|\*\|(asn|ipv4|ipv6)\|\*\|([0-9]+)\|summary`)
	matches := re.FindStringSubmatch(line)
	if matches != nil {
		switch matches[2] {
		case "ipv4":
			hdr.ipv4Count, _ = strconv.ParseUint(matches[3], 10, 64)
		case "asn":
			hdr.asnCount, _ = strconv.ParseUint(matches[3], 10, 64)
		case "ipv6":
			hdr.ipv6Count, _ = strconv.ParseUint(matches[3], 10, 64)
		default:
			panic("Unknown record type: " + matches[2])
		}
		verbosePrint(3, fmt.Sprintf("HEADER FIELDS: %d::%d::%d\n", hdr.ipv4Count, hdr.asnCount, hdr.ipv6Count))
		verbosePrint(4, fmt.Sprintf("%q\n", matches))
	} else {
		verbosePrint(3, "NO HEADER MATCHES")
	}
}

func saveHeaderData(db *sql.DB, hdr FileHeader) int64 {
	var lastID int64
	verbosePrint(2, "Saving header data in database.\n")
	verbosePrint(3, fmt.Sprintf("INSERT INTO Datasets VALUES( DEFAULT, %d, %d, %s, %d, %s, %s, %d)", hdr.registry, hdr.serial, hdr.version, hdr.records, hdr.startdate, hdr.enddate, hdr.UTCoffset))
	res, err := db.Exec("INSERT INTO Datasets VALUES( DEFAULT, ?, ?, ?, ?, ?, ?, ?)",
		hdr.registry, hdr.serial, hdr.version, hdr.records, hdr.startdate, hdr.enddate, hdr.UTCoffset)

	if err == nil { // Error may be caused by duplicated unique indexes so attempt to do a select query to see if there is a match
		lastID, err = res.LastInsertId()
		//raf, err := res.RowsAffected()
	} else {
		driverErr, _ := err.(*mysql.MySQLError)
		if driverErr.Number == 1062 && *f_force { // Duplicate entry and force enable; continuing
			verbosePrint(2, "Warning: Unable to insert Dataset; probably a duplicate... quering database for an earlier copy.")
			err = db.QueryRow("SELECT ID FROM Datasets WHERE ID_Registries = ? AND serial = ?;", hdr.registry, hdr.serial).Scan(&lastID)
			if err != nil {
				log.Fatal(err)
			}
		} else {
			log.Fatal(err)
		}
	}

	summaries := map[string]*uint64{
		"ipv4": &hdr.ipv4Count,
		"asn":  &hdr.asnCount,
		"ipv6": &hdr.ipv6Count,
	}

	for k := range summaries {
		res, err = db.Exec("INSERT INTO Summaries VALUES( DEFAULT, ?, ?, ?, ?)", lastID, k, summaries[k], hdr.enddate)
		if err != nil {
			verbosePrint(2, fmt.Sprintf("Warning: cannot record summary value for %s: %s\n", k, err.Error()))
		}
	}
	return lastID
}

func parseHeader(scanner *bufio.Scanner, hdr *FileHeader) {
	verbosePrint(2, "Parsing header.\n")

	//Read first header line
	scanner.Scan()
	line := scanner.Text()

	// Skip all comments
	for line[0] == '#' || line[0] == '\r' { // APNIC has a bunch of comments in the file before the header starts so skip them
		fmt.Println(line)
		scanner.Scan()
		line = scanner.Text()
	}

	if parseVersionLine(hdr, line) { // Read next 3 lines
		for i := 0; i < 3 && scanner.Scan(); i++ {
			line := scanner.Text()
			parseSummaryLine(hdr, line)
		}
	}
}

func parseData(db *sql.DB, data []byte) { // r io.Reader
	var hdr FileHeader
	var lastID int64

	r := bytes.NewReader(data)
	scanner := bufio.NewScanner(r)

	parseHeader(scanner, &hdr)
	lastID = saveHeaderData(db, hdr)

	queryTempl := "INSERT INTO %s VALUES ( DEFAULT, %d, ?, ?, %s, ?, ?, ?, ?, ?, %s)"
	var ipv4Query, asnQuery, ipv6Query sql.Stmt

	recordTypes := map[string]*sql.Stmt{
		"ipv4": &ipv4Query,
		"asn":  &asnQuery,
		"ipv6": &ipv6Query,
	}

	verbosePrint(3, "DEBUG: Preparing DB queries.\n")
	for k := range recordTypes {
		var conversion = "?"
		if k == "ipv4" {
			conversion = "INET_ATON(?)"
		}
		if k == "ipv6" {
			conversion = "INET6_ATON(?)"
		}
		stmt, err := db.Prepare(fmt.Sprintf(queryTempl, "Records_"+string(k), lastID, conversion, hdr.enddate))
		recordTypes[k] = stmt
		verbosePrint(3, fmt.Sprintf("DEBUG: Query: "+string(queryTempl)+"\n", "Records_"+string(k), lastID, conversion, hdr.enddate))

		if err != nil {
			fmt.Printf("Warning: prepare query for %s: %s\n", k, err.Error())
		}
		defer recordTypes[k].Close()
	}

	// NOTE: It is not possible to start parsing records until the header is parsed because he "insertion date" is taken from the header
	// Read records
	verbosePrint(2, "Processing records.\n")
	//var counter int64
	//"ipv4": &ipv4Query,
	//"asn":  &asnQuery,
	//"ipv6": &ipv6Query,

	var counter = map[string]uint64{
		"ipv4":    0,
		"asn":     0,
		"ipv6":    0,
		"all":     0,
		"invalid": 0,
	}
	for counter["all"] = 0; scanner.Scan(); counter["all"]++ {
		line := scanner.Text()
		verbosePrint(4, fmt.Sprintf("RECORD: line: %s\n", line)) // Println will add back the final '\n'

		re := regexp.MustCompile(`^(afrinic|apnic|arin|lacnic|ripencc)\|([A-Z].|)\|(asn|ipv4|ipv6)\|([0-9a-f:.]+)\|([0-9]+)\|([0-9]+|)\|(allocated|assigned|available|reserved)(.*)$`)

		matches := re.FindStringSubmatch(line)
		if matches != nil {
			if matches[6] == "00000000" || matches[6] == "" { // ARIN dataset artifact: replace with NULL
				matches[6] = "1970-01-01"
			}
			verbosePrint(4, fmt.Sprintf("RECORD FIELDS: %s:%s:%s:%s:%s:%s:%s:%s\n", matches[1], matches[2], matches[4], matches[5], matches[6], matches[7], matches[8], ""))
			_, err := recordTypes[matches[3]].Exec(matches[1], matches[2], matches[4], matches[5], matches[6], matches[7], matches[8], "")
			if err != nil {
				driverErr, _ := err.(*mysql.MySQLError)
				if !(driverErr.Number == 1062 && *f_force) {
					verbosePrint(2, fmt.Sprintf("Warning: EXEC: %s: %s => %q\n", matches[3], err.Error(), matches[1], matches[2], matches[4], matches[5], matches[6], matches[7], matches[8], ""))
				}
			}
			counter[matches[3]]++
		} else {
			verbosePrint(3, fmt.Sprintf("DEBUG: INVALID RECORD: %s\n", line))
			counter["invalid"]++
		}
		if counter["all"]%5000 == 0 {
			verbosePrint(2, fmt.Sprintf("%d records complete...\n", counter["all"]))
		}
	}
	verbosePrint(2, fmt.Sprintf("Processed %d records.\nASN: %d\nIPv4: %d\nIPv6: %d\nInvalid: %d\n", counter["all"], counter["asn"], counter["ipv4"], counter["ipv6"], counter["invalid"]))

	if err := scanner.Err(); err != nil {
		fmt.Fprintln(os.Stderr, "reading standard input:", err)
	}

}

func downloadFile(url *string) []byte {

	verbosePrint(1, fmt.Sprintf("Downloading file from: %s\n", *url))

	http_session, err := http.Get(*url)
	if err != nil {
		log.Fatal(err)
	}
	buffer, err := ioutil.ReadAll(http_session.Body)
	if err != nil {
		log.Fatal(err)
	}
	http_session.Body.Close()

	verbosePrint(2, fmt.Sprintf("Download complete. Downloaded %d bytes.\n", len(buffer)))

	return buffer
}

func main() {
	// Parse command line arguments
	parseArguments()

	// Setup and test database connection
	db := setupDB()
	defer db.Close()

	switch *f_source {
	case "file":
		verbosePrint(1, fmt.Sprintf("Reading from: %s\n", *f_inputFileName))
		data, err := ioutil.ReadFile(*f_inputFileName)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ERROR: reading data file %s.", *f_inputFileName)
			log.Fatal(err)
		}
		verbosePrint(2, "File read complete.\n")
		parseData(db, data)

	case "afrinic":
		fallthrough
	case "apnic":
		fallthrough
	case "arin":
		fallthrough
	case "lacnic":
		fallthrough
	case "ripencc":
		*f_URL = getRegistryURL(db, *f_source)
		fallthrough
	case "download":
		data := downloadFile(f_URL)
		//parseData(db, bytes.NewReader(data))
		parseData(db, data)
	case "all":
		registries := []string{"afrinic", "apnic", "arin", "lacnic", "ripencc"}
		for _, reg := range registries {
			fmt.Println("Processing: " + reg)
			url := getRegistryURL(db, reg)
			data := downloadFile(&url)
			parseData(db, data)
		}

	default:
		log.Fatal("Invalid source type: " + *f_source)
	}

}

func getRegistryURL(db *sql.DB, registry string) string {
	var URL string
	err := db.QueryRow("SELECT LatestDataSetLocation FROM Registries WHERE ShortName = ?;", registry).Scan(&URL)
	if err != nil {
		log.Fatal(err)
	}

	verbosePrint(3, fmt.Sprintf("DEBUG: Looked up registry URL for %s: %s\n", registry, URL))

	return URL
}

func parseArguments() {
	f_inputFileName = flag.String("in", "", "Use input file instead of downloading. Overrides flag -registry.")
	f_URL = flag.String("url", "", "URL to download the data. Overrides flag -registry.")
	f_source = flag.String("source", "", "Registry to download using default location. Can be one of: all, afrinic, apnic, arin, lacnic, ripencc, as well as file and download.")

	f_verbose = flag.Uint("verbose", 1, "Verboseness level; 0 - errors only; 1 - normal output; 3 - debug")
	f_debug = flag.Bool("debug", false, "Debug (true/false); sets verboseness to 5.")
	f_force = flag.Bool("force", false, "Forces data import even if Dataset and Summary records exist for the import (true/false)")
	f_invalid_hdr_ok = flag.Bool("invalid-header-ok", false, "Ignore invalid header (true/false)")

	flag.Parse()

	if *f_URL != "" && *f_inputFileName != "" && *f_source == "" {
		log.Fatal("Only URL or input file can be set.")
	}
	if *f_source == "" && *f_inputFileName != "" {
		*f_source = "file"
	}
	if *f_source == "" && *f_URL != "" {
		*f_source = "download"
	}
	if *f_source == "file" && *f_inputFileName == "" {
		log.Fatal("Please, specify a filename using \"-in\".")
	}
	if *f_source == "download" && *f_URL == "" {
		log.Fatal("Please, specify a webresource using \"-url\".")
	}
	if *f_debug {
		*f_verbose = 5
	}
	if *f_verbose >= 3 && len(flag.Args()) > 0 {
		fmt.Println(os.Stderr, "Unprocessed args:", flag.Args())
	}
}

func verbosePrint(level uint, message string) {
	if level <= *f_verbose {
		/*		if a == nil {
					fmt.Printf(format)
				} else {
					fmt.Printf(format, a)
				}*/
		fmt.Print(message)
	}
}

func setupDB() *sql.DB {
	// Get username password from ENV variables
	user := GetEnvDef("MYSQL_USER", "root")
	pass := GetEnvDef("MYSQL_PASS", "")
	prot := GetEnvDef("MYSQL_PROT", "tcp")
	addr := GetEnvDef("MYSQL_ADDR", "localhost:3306")
	dbname := GetEnvDef("MYSQL_DBNAME", "ip2asn")
	dsn := fmt.Sprintf("%s:%s@%s(%s)/%s?timeout=15s", user, pass, prot, addr, dbname)

	db, err := sql.Open("mysql", dsn)
	if err != nil {
		log.Fatal(err.Error())
	}
	err = db.Ping()
	if err != nil {
		log.Fatal(err.Error())
	}
	return db
}

func GetEnvDef(envvar string, default_val string) string {
	value := os.Getenv(envvar)
	if value == "" { // Set default value
		return default_val
	}
	return value
}
