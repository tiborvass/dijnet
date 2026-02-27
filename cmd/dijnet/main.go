package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/ProZsolt/dijnet"
)

const dateLayout = "2006-01-02"

type config struct {
	username      string
	password      string
	invoicePath   string
	provider      string
	issuerID      string
	from          string
	to            string
	resume        bool
	redownload    bool
	listProviders bool
	listInvoices  bool
	downloadPDF   bool
	downloadXML   bool
}

var invoiceFilenameDateRegex = regexp.MustCompile(`^(\d{4}-\d{2}-\d{2})_.*\.(pdf|xml)$`)

func main() {
	cfg := parseFlags()

	if err := run(cfg); err != nil {
		log.Fatal(err)
	}
}

func parseFlags() config {
	cfg := config{}

	flag.StringVar(&cfg.username, "username", os.Getenv("DIJNET_USERNAME"), "Dijnet username (or DIJNET_USERNAME)")
	flag.StringVar(&cfg.password, "password", os.Getenv("DIJNET_PASSWORD"), "Dijnet password (or DIJNET_PASSWORD)")
	flag.StringVar(&cfg.invoicePath, "invoice-path", envOrDefault("DIJNET_INVOICE_PATH", "invoices"), "Base directory where invoices are stored")
	flag.StringVar(&cfg.provider, "provider", "", "Provider name filter (exact or case-insensitive match)")
	flag.StringVar(&cfg.issuerID, "issuer-id", "", "Issuer ID filter")
	flag.StringVar(&cfg.from, "from", "", "From issue date, inclusive (YYYY-MM-DD)")
	flag.StringVar(&cfg.to, "to", "", "To issue date, inclusive (YYYY-MM-DD)")
	flag.BoolVar(&cfg.resume, "resume", false, "Automatically resume from one day after the newest local invoice date")
	flag.BoolVar(&cfg.redownload, "redownload", false, "Ignore local invoice history and redownload all matching invoices")
	flag.BoolVar(&cfg.listProviders, "list-providers", false, "List available providers and exit")
	flag.BoolVar(&cfg.listInvoices, "list-invoices", false, "List matching invoices but do not download")
	flag.BoolVar(&cfg.downloadPDF, "download-pdf", true, "Download PDF files")
	flag.BoolVar(&cfg.downloadXML, "download-xml", true, "Download XML files")

	flag.Parse()
	return cfg
}

func run(cfg config) error {
	if cfg.username == "" || cfg.password == "" {
		return errors.New("missing credentials: set --username/--password or DIJNET_USERNAME/DIJNET_PASSWORD")
	}
	if cfg.resume && cfg.redownload {
		return errors.New("--resume and --redownload are mutually exclusive")
	}

	from, err := parseDateFlag("from", cfg.from)
	if err != nil {
		return err
	}
	to, err := parseDateFlag("to", cfg.to)
	if err != nil {
		return err
	}
	if !from.IsZero() && !to.IsZero() && from.After(to) {
		return fmt.Errorf("invalid range: --from (%s) is after --to (%s)", from.Format(dateLayout), to.Format(dateLayout))
	}

	if !cfg.downloadPDF && !cfg.downloadXML && !cfg.listInvoices && !cfg.listProviders {
		return errors.New("nothing to do: enable --download-pdf and/or --download-xml, or use --list-invoices/--list-providers")
	}

	srv := dijnet.NewService()
	if err := srv.Login(cfg.username, cfg.password); err != nil {
		return fmt.Errorf("login error: %w", err)
	}

	providers, token, err := srv.Providers()
	if err != nil {
		return fmt.Errorf("unable to get providers: %w", err)
	}

	if cfg.listProviders {
		for _, p := range providers {
			fmt.Println(p)
		}
		if cfg.provider == "" {
			return nil
		}
	}

	latestLocalInvoiceDate, hasLocalInvoices, err := latestInvoiceDate(cfg.invoicePath)
	if err != nil {
		return err
	}
	if cfg.redownload {
		from = time.Time{}
		to = time.Time{}
	} else {
		shouldResume := cfg.resume
		if !cfg.resume && hasLocalInvoices {
			shouldResume, err = promptResume(latestLocalInvoiceDate)
			if err != nil {
				return err
			}
		}
		if shouldResume && hasLocalInvoices {
			from = latestLocalInvoiceDate.AddDate(0, 0, 1)
		}
	}

	provider, err := resolveProvider(cfg.provider, providers)
	if err != nil {
		return err
	}

	query := dijnet.InvoicesQuery{
		Provider: provider,
		IssuerID: cfg.issuerID,
		From:     from,
		To:       to,
		Token:    token,
	}

	invoices, err := srv.Invoices(query)
	if err != nil {
		return fmt.Errorf("unable to get invoices: %w", err)
	}

	if len(invoices) == 0 {
		fmt.Println("No invoices found for the selected filters")
		return nil
	}

	if cfg.listInvoices {
		for _, inv := range invoices {
			fmt.Printf("%s | %s | %s | %s\n", inv.DateOfIssue.Format(dateLayout), inv.Provider, inv.IssuerID, inv.InvoiceID)
		}
		if !cfg.downloadPDF && !cfg.downloadXML {
			return nil
		}
	}

	for i, invoice := range invoices {
		fmt.Printf("Downloading invoice %d/%d\n", i+1, len(invoices))

		providerPath := filepath.Join(cfg.invoicePath, invoice.Provider, invoice.IssuerID)
		if err := os.MkdirAll(providerPath, os.ModePerm); err != nil {
			return fmt.Errorf("unable to create directory %s: %w", providerPath, err)
		}

		invoiceFilename := invoice.DateOfIssue.Format(dateLayout) + "_" + strings.ReplaceAll(invoice.InvoiceID, "/", "_")
		pdfPath := ""
		xmlPath := ""
		if cfg.downloadPDF {
			pdfPath = filepath.Join(providerPath, invoiceFilename+".pdf")
		}
		if cfg.downloadXML {
			xmlPath = filepath.Join(providerPath, invoiceFilename+".xml")
		}

		if err := srv.DownloadInvoice(invoice, pdfPath, xmlPath); err != nil {
			return fmt.Errorf("download failed for invoice %s: %w", invoice.InvoiceID, err)
		}
	}

	return nil
}

func latestInvoiceDate(basePath string) (time.Time, bool, error) {
	info, err := os.Stat(basePath)
	if err != nil {
		if os.IsNotExist(err) {
			return time.Time{}, false, nil
		}
		return time.Time{}, false, fmt.Errorf("unable to inspect invoice path %s: %w", basePath, err)
	}
	if !info.IsDir() {
		return time.Time{}, false, fmt.Errorf("invoice path %s is not a directory", basePath)
	}

	latest := time.Time{}
	found := false
	walkErr := filepath.WalkDir(basePath, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}

		matches := invoiceFilenameDateRegex.FindStringSubmatch(strings.ToLower(filepath.Base(path)))
		if len(matches) < 2 {
			return nil
		}

		date, parseErr := time.Parse(dateLayout, matches[1])
		if parseErr != nil {
			return nil
		}
		if !found || date.After(latest) {
			latest = date
			found = true
		}

		return nil
	})
	if walkErr != nil {
		return time.Time{}, false, fmt.Errorf("unable to walk invoice path %s: %w", basePath, walkErr)
	}

	return latest, found, nil
}

func promptResume(latestDate time.Time) (bool, error) {
	resumeFrom := latestDate.AddDate(0, 0, 1).Format(dateLayout)
	fmt.Printf("Existing invoices detected up to %s. Resume from %s? [Y/n]: ", latestDate.Format(dateLayout), resumeFrom)

	reader := bufio.NewReader(os.Stdin)
	input, err := reader.ReadString('\n')
	if err != nil {
		return false, fmt.Errorf("unable to read prompt input: %w", err)
	}

	answer := strings.ToLower(strings.TrimSpace(input))
	return answer == "" || answer == "y" || answer == "yes", nil
}

func parseDateFlag(name, value string) (time.Time, error) {
	if value == "" {
		return time.Time{}, nil
	}

	t, err := time.Parse(dateLayout, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid --%s date %q, expected %s", name, value, dateLayout)
	}
	return t, nil
}

func resolveProvider(input string, providers []string) (string, error) {
	if input == "" {
		return "", nil
	}

	for _, p := range providers {
		if p == input {
			return p, nil
		}
	}

	inputLower := strings.ToLower(strings.TrimSpace(input))
	if inputLower == "" {
		return "", nil
	}

	for _, p := range providers {
		if strings.ToLower(p) == inputLower {
			return p, nil
		}
	}

	for _, p := range providers {
		if strings.Contains(strings.ToLower(p), inputLower) {
			return p, nil
		}
	}

	return "", fmt.Errorf("provider %q not found; use --list-providers to inspect valid names", input)
}

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
