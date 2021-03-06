// Copyright 2017 Jeff Foley. All rights reserved.
// Use of this source code is governed by Apache 2 LICENSE that can be found in the LICENSE file.

package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"strings"
	"time"

	"github.com/OWASP/Amass/v3/config"
	"github.com/OWASP/Amass/v3/eventbus"
	"github.com/OWASP/Amass/v3/format"
	"github.com/OWASP/Amass/v3/requests"
	"github.com/OWASP/Amass/v3/resolvers"
	"github.com/OWASP/Amass/v3/services"
	"github.com/OWASP/Amass/v3/stringset"
	"github.com/fatih/color"
	"github.com/miekg/dns"
)

const (
	dnsUsageMsg = "dns [options]"
)

type dnsArgs struct {
	Blacklist     stringset.Set
	Domains       stringset.Set
	MaxDNSQueries int
	Names         stringset.Set
	RecordTypes   stringset.Set
	Resolvers     stringset.Set
	Timeout       int
	Options       struct {
		DemoMode            bool
		IPs                 bool
		IPv4                bool
		IPv6                bool
		MonitorResolverRate bool
		Unresolved          bool
		Verbose             bool
	}
	Filepaths struct {
		AllFilePrefix string
		Blacklist     string
		ConfigFile    string
		Directory     string
		Domains       format.ParseStrings
		JSONOutput    string
		LogFile       string
		Names         format.ParseStrings
		Resolvers     format.ParseStrings
		TermOut       string
	}
}

func defineDNSArgumentFlags(dnsFlags *flag.FlagSet, args *dnsArgs) {
	dnsFlags.Var(&args.Blacklist, "bl", "Blacklist of subdomain names that will not be investigated")
	dnsFlags.Var(&args.Domains, "d", "Domain names separated by commas (can be used multiple times)")
	dnsFlags.IntVar(&args.MaxDNSQueries, "max-dns-queries", 0, "Maximum number of concurrent DNS queries")
	dnsFlags.Var(&args.RecordTypes, "t", "DNS record types to be queried for (can be used multiple times)")
	dnsFlags.Var(&args.Resolvers, "r", "IP addresses of preferred DNS resolvers (can be used multiple times)")
	dnsFlags.IntVar(&args.Timeout, "timeout", 0, "Number of minutes to let enumeration run before quitting")
}

func defineDNSOptionFlags(dnsFlags *flag.FlagSet, args *dnsArgs) {
	dnsFlags.BoolVar(&args.Options.DemoMode, "demo", false, "Censor output to make it suitable for demonstrations")
	dnsFlags.BoolVar(&args.Options.IPs, "ip", false, "Show the IP addresses for discovered names")
	dnsFlags.BoolVar(&args.Options.IPv4, "ipv4", false, "Show the IPv4 addresses for discovered names")
	dnsFlags.BoolVar(&args.Options.IPv6, "ipv6", false, "Show the IPv6 addresses for discovered names")
	dnsFlags.BoolVar(&args.Options.MonitorResolverRate, "noresolvrate", true, "Disable resolver rate monitoring")
	dnsFlags.BoolVar(&args.Options.Unresolved, "include-unresolvable", false, "Output DNS names that did not resolve")
	dnsFlags.BoolVar(&args.Options.Verbose, "v", false, "Output status / debug / troubleshooting info")
}

func defineDNSFilepathFlags(dnsFlags *flag.FlagSet, args *dnsArgs) {
	dnsFlags.StringVar(&args.Filepaths.AllFilePrefix, "oA", "", "Path prefix used for naming all output files")
	dnsFlags.StringVar(&args.Filepaths.Blacklist, "blf", "", "Path to a file providing blacklisted subdomains")
	dnsFlags.StringVar(&args.Filepaths.ConfigFile, "config", "", "Path to the INI configuration file. Additional details below")
	dnsFlags.StringVar(&args.Filepaths.Directory, "dir", "", "Path to the directory containing the output files")
	dnsFlags.Var(&args.Filepaths.Domains, "df", "Path to a file providing root domain names")
	dnsFlags.StringVar(&args.Filepaths.JSONOutput, "json", "", "Path to the JSON output file")
	dnsFlags.StringVar(&args.Filepaths.LogFile, "log", "", "Path to the log file where errors will be written")
	dnsFlags.Var(&args.Filepaths.Names, "nf", "Path to a file providing already known subdomain names (from other tools/sources)")
	dnsFlags.Var(&args.Filepaths.Resolvers, "rf", "Path to a file providing preferred DNS resolvers")
	dnsFlags.StringVar(&args.Filepaths.TermOut, "o", "", "Path to the text file containing terminal stdout/stderr")
}

func runDNSCommand(clArgs []string) {
	args := dnsArgs{
		Blacklist:   stringset.New(),
		Domains:     stringset.New(),
		Names:       stringset.New(),
		RecordTypes: stringset.New(),
		Resolvers:   stringset.New(),
	}
	var help1, help2 bool
	dnsCommand := flag.NewFlagSet("dns", flag.ContinueOnError)

	dnsBuf := new(bytes.Buffer)
	dnsCommand.SetOutput(dnsBuf)

	dnsCommand.BoolVar(&help1, "h", false, "Show the program usage message")
	dnsCommand.BoolVar(&help2, "help", false, "Show the program usage message")
	defineDNSArgumentFlags(dnsCommand, &args)
	defineDNSOptionFlags(dnsCommand, &args)
	defineDNSFilepathFlags(dnsCommand, &args)

	if len(clArgs) < 1 {
		commandUsage(dnsUsageMsg, dnsCommand, dnsBuf)
		return
	}

	if err := dnsCommand.Parse(clArgs); err != nil {
		r.Fprintf(color.Error, "%v\n", err)
		os.Exit(1)
	}
	if help1 || help2 {
		commandUsage(dnsUsageMsg, dnsCommand, dnsBuf)
		return
	}

	if err := processDNSInputFiles(&args); err != nil {
		fmt.Fprintf(color.Error, "%v\n", err)
		os.Exit(1)
	}

	cfg := config.NewConfig()
	// Check if a configuration file was provided, and if so, load the settings
	if err := config.AcquireConfig(args.Filepaths.Directory, args.Filepaths.ConfigFile, cfg); err == nil {
		// Check if a config file was provided that has DNS resolvers specified
		if len(cfg.Resolvers) > 0 && len(args.Resolvers) == 0 {
			args.Resolvers = stringset.New(cfg.Resolvers...)
		}
	} else if args.Filepaths.ConfigFile != "" {
		r.Fprintf(color.Error, "Failed to load the configuration file: %v\n", err)
		os.Exit(1)
	}

	// Override configuration file settings with command-line arguments
	if err := cfg.UpdateConfig(args); err != nil {
		r.Fprintf(color.Error, "Configuration error: %v\n", err)
		os.Exit(1)
	}

	createOutputDirectory(cfg)

	// Seed the default pseudo-random number generator
	rand.Seed(time.Now().UTC().UnixNano())

	sys, err := services.NewLocalSystem(cfg)
	if err != nil {
		r.Fprintf(color.Error, "%v\n", err)
		os.Exit(1)
	}

	performResolutions(cfg, sys)
}

func performResolutions(cfg *config.Config, sys services.System) {
	done := make(chan struct{})
	active := make(chan struct{}, 1000000)
	bus := eventbus.NewEventBus(10000)
	answers := make(chan *requests.DNSRequest, 100000)

	// Setup the context used throughout the resolutions
	ctx, cancel := context.WithCancel(context.Background())
	ctx = context.WithValue(ctx, requests.ContextEventBus, bus)

	if cfg.Timeout > 0 {
		time.AfterFunc(time.Duration(cfg.Timeout)*time.Minute, func() {
			close(done)
		})
	}

	activeFunc := func(s string) { active <- struct{}{} }
	resolvFunc := func(t time.Time, rcode int) { active <- struct{}{} }
	bus.Subscribe(requests.SetActiveTopic, activeFunc)
	defer bus.Unsubscribe(requests.SetActiveTopic, activeFunc)
	bus.Subscribe(requests.ResolveCompleted, resolvFunc)
	defer bus.Unsubscribe(requests.ResolveCompleted, resolvFunc)

	go func() {
		for _, name := range cfg.ProvidedNames {
			select {
			case <-done:
				cancel()
				return
			default:
				cfg.SemMaxDNSQueries.Acquire(1)
				go processDNSRequest(ctx, &requests.DNSRequest{Name: name}, cfg, sys, answers)
			}
		}
	}()

	processDNSAnswers(cfg, active, answers, done)
}

func processDNSRequest(ctx context.Context, req *requests.DNSRequest,
	cfg *config.Config, sys services.System, c chan *requests.DNSRequest) {
	defer cfg.SemMaxDNSQueries.Release(1)

	if req == nil || req.Name == "" {
		c <- nil
		return
	}

	req.Domain = sys.Pool().SubdomainToDomain(req.Name)
	if req.Domain == "" {
		c <- nil
		return
	}

	if cfg.Blacklisted(req.Name) || sys.Pool().GetWildcardType(ctx, req) == resolvers.WildcardTypeDynamic {
		c <- nil
		return
	}

	var answers []requests.DNSAnswer
	for _, t := range cfg.RecordTypes {
		a, _, err := sys.Pool().Resolve(ctx, req.Name, t, resolvers.PriorityLow)
		if err == nil {
			answers = append(answers, a...)
		}

		if t == "CNAME" && len(answers) > 0 {
			break
		}
	}
	req.Records = answers

	if len(req.Records) == 0 || sys.Pool().MatchesWildcard(ctx, req) {
		c <- nil
		return
	}

	c <- req
}

func processDNSAnswers(cfg *config.Config,
	activeChan chan struct{}, answers chan *requests.DNSRequest, done chan struct{}) {
	first := true
	active := true

	t := time.NewTicker(5 * time.Second)
	defer t.Stop()

	l := len(cfg.ProvidedNames)
	for i := 0; i < l; {
		select {
		case <-done:
			return
		case <-t.C:
			if first {
				continue
			} else if active {
				active = false
				continue
			}
			return
		case <-activeChan:
			active = true
		case req := <-answers:
			i++
			active = true
			first = false

			if req != nil && len(req.Records) != 0 {
				tss := stringset.New()
				for _, rec := range req.Records {
					tss.Insert(typeToName(uint16(rec.Type)))
				}

				var ts string
				for j, str := range tss.Slice() {
					if j != 0 {
						ts += ", "
					}
					ts += strings.ToUpper(str)
				}
				tstr := fmt.Sprintf("%-24s", "["+ts+"] ")

				var data string
				for j, rec := range req.Records {
					if j != 0 {
						data += ", "
					}

					if uint16(rec.Type) == dns.TypeNS {
						rec.Data = strings.Split(rec.Data, ",")[1]
					}
					data += resolvers.RemoveLastDot(rec.Data)
				}

				fmt.Fprintf(color.Output, "%s%s %s\n", blue(tstr), green(req.Name), yellow(data))
			}
		}
	}
}

// Obtain parameters from provided input files
func processDNSInputFiles(args *dnsArgs) error {
	if args.Filepaths.Blacklist != "" {
		list, err := config.GetListFromFile(args.Filepaths.Blacklist)
		if err != nil {
			return fmt.Errorf("Failed to parse the blacklist file: %v", err)
		}
		args.Blacklist.InsertMany(list...)
	}
	if len(args.Filepaths.Names) > 0 {
		for _, f := range args.Filepaths.Names {
			list, err := config.GetListFromFile(f)
			if err != nil {
				return fmt.Errorf("Failed to parse the subdomain names file: %v", err)
			}

			args.Names.InsertMany(list...)
		}
	}
	if len(args.Filepaths.Domains) > 0 {
		for _, f := range args.Filepaths.Domains {
			list, err := config.GetListFromFile(f)
			if err != nil {
				return fmt.Errorf("Failed to parse the domain names file: %v", err)
			}

			args.Domains.InsertMany(list...)
		}
	}
	if len(args.Filepaths.Resolvers) > 0 {
		for _, f := range args.Filepaths.Resolvers {
			list, err := config.GetListFromFile(f)
			if err != nil {
				return fmt.Errorf("Failed to parse the esolver file: %v", err)
			}

			args.Resolvers.InsertMany(list...)
		}
	}
	return nil
}

// Setup the amass DNS settings
func (d dnsArgs) OverrideConfig(conf *config.Config) error {
	if d.Filepaths.Directory != "" {
		conf.Dir = d.Filepaths.Directory
	}
	if d.MaxDNSQueries > 0 {
		conf.MaxDNSQueries = d.MaxDNSQueries
	}
	if len(d.Names) > 0 {
		conf.ProvidedNames = d.Names.Slice()
	}
	if d.Options.Unresolved {
		conf.IncludeUnresolvable = true
	}
	if len(d.Blacklist) > 0 {
		conf.Blacklist = d.Blacklist.Slice()
	}
	if d.Timeout > 0 {
		conf.Timeout = d.Timeout
	}
	if d.RecordTypes.Len() > 0 {
		conf.RecordTypes = d.RecordTypes.Slice()

		for i, qtype := range conf.RecordTypes {
			conf.RecordTypes[i] = strings.ToUpper(qtype)

			if conf.RecordTypes[i] == "CNAME" {
				tmp := conf.RecordTypes[0]

				conf.RecordTypes[0] = conf.RecordTypes[i]
				conf.RecordTypes[i] = tmp
			}
		}
	} else {
		conf.RecordTypes = []string{"A"}
	}
	if d.Resolvers.Len() > 0 {
		conf.SetResolvers(d.Resolvers.Slice())
	}
	if !d.Options.MonitorResolverRate {
		conf.MonitorResolverRate = false
	}

	// Attempt to add the provided domains to the configuration
	conf.AddDomains(d.Domains.Slice())
	return nil
}

func typeToName(qtype uint16) string {
	var name string

	switch qtype {
	case dns.TypeCNAME:
		name = "CNAME"
	case dns.TypeA:
		name = "A"
	case dns.TypeAAAA:
		name = "AAAA"
	case dns.TypePTR:
		name = "PTR"
	case dns.TypeNS:
		name = "NS"
	case dns.TypeMX:
		name = "MX"
	case dns.TypeTXT:
		name = "TXT"
	case dns.TypeSOA:
		name = "SOA"
	case dns.TypeSPF:
		name = "SPF"
	case dns.TypeSRV:
		name = "SRV"
	}

	return name
}
