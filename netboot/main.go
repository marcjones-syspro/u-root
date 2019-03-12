package main

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/insomniacslk/dhcp/dhcpv6"
	"github.com/insomniacslk/dhcp/iana"
	"github.com/insomniacslk/dhcp/interfaces"
	"github.com/insomniacslk/dhcp/netboot"
	"github.com/systemboot/systemboot/pkg/crypto"
	"github.com/u-root/u-root/pkg/kexec"
)

var (
	useV4              = flag.Bool("4", false, "Get a DHCPv4 lease")
	useV6              = flag.Bool("6", true, "Get a DHCPv6 lease")
	ifname             = flag.String("i", "", "Interface to send packets through")
	dryRun             = flag.Bool("dryrun", false, "Do everything except assigning IP addresses, changing DNS, and kexec")
	doDebug            = flag.Bool("d", false, "Print debug output")
	skipDHCP           = flag.Bool("skip-dhcp", false, "Skip DHCP and rely on SLAAC for network configuration. This requires -netboot-url")
	overrideNetbootURL = flag.String("netboot-url", "", "Override the netboot URL normally obtained via DHCP")
	readTimeout        = flag.Int("timeout", 3, "Read timeout in seconds")
	dhcpRetries        = flag.Int("retries", 3, "Number of times a DHCP request is retried")
	userClass          = flag.String("userclass", "", "Override DHCP User Class option")
	caCertFile         = flag.String("cacerts", "", "Base64 encoded CA cert file")
	skipCertVerify     = flag.Bool("skip-cert-verify", false, "Don't authenticate https certs")
)

const (
	interfaceUpTimeout = 10 * time.Second
	maxHTTPAttempts    = 3
	retryInterval      = time.Second
)

var banner = `

 _________________________________
< Net booting is so hot right now >
 ---------------------------------
        \   ^__^
         \  (oo)\_______
            (__)\       )\/\
                ||----w |
                ||     ||

`
var debug = func(string, ...interface{}) {}

func main() {
	flag.Parse()
	if *skipDHCP && *overrideNetbootURL == "" {
		log.Fatal("-skip-dhcp requires -netboot-url")
	}
	if *doDebug {
		debug = log.Printf
	}
	log.Print(banner)

	if !*useV6 && !*useV4 {
		log.Fatal("At least one of DHCPv6 and DHCPv4 is required")
	}

	iflist := []net.Interface{}
	if *ifname != "" {
		var iface *net.Interface
		var err error
		if iface, err = net.InterfaceByName(*ifname); err != nil {
			log.Fatalf("Could not find interface %s: %v", *ifname, err)
		}
		iflist = append(iflist, *iface)
	} else {
		var err error
		if iflist, err = interfaces.GetNonLoopbackInterfaces(); err != nil {
			log.Fatalf("Could not obtain the list of network interfaces: %v", err)
		}
	}

	for _, iface := range iflist {
		log.Printf("Waiting for network interface %s to come up", iface.Name)
		start := time.Now()
		_, err := netboot.IfUp(iface.Name, interfaceUpTimeout)
		if err != nil {
			log.Printf("IfUp failed: %v", err)
			continue
		}
		debug("Interface %s is up after %v", iface.Name, time.Since(start))

		var dhcp []dhcpFunc
		if *useV6 {
			dhcp = append(dhcp, dhcp6)
		}
		if *useV4 {
			dhcp = append(dhcp, dhcp4)
		}
		for _, d := range dhcp {
			if err := boot(iface.Name, d); err != nil {
				log.Printf("Could not boot from %s: %v", iface.Name, err)
			}
		}
	}

	log.Fatalln("Could not boot from any interfaces")
}

func retryableNetError(err error) bool {
	if err == nil {
		return false
	}
	switch err := err.(type) {
	case net.Error:
		if err.Timeout() {
			return true
		}
	}
	return false
}

func retryableHTTPError(resp *http.Response) bool {
	if resp == nil {
		return false
	}
	if resp.StatusCode == 500 || resp.StatusCode == 502 {
		return true
	}
	return false
}

func boot(ifname string, dhcp dhcpFunc) error {
	var (
		netconf  *netboot.NetConf
		bootfile string
		err      error
	)
	if *skipDHCP {
		log.Print("Skipping DHCP")
	} else {
		// send a netboot request via DHCP
		netconf, bootfile, err = dhcp(ifname)
		if err != nil {
			return fmt.Errorf("DHCPv6: netboot request for interface %s failed: %v", ifname, err)
		}
		debug("DHCP: network configuration: %+v", netconf)
		if !*dryRun {
			log.Printf("DHCP: configuring network interface %s with %v", ifname, netconf)
			if err = netboot.ConfigureInterface(ifname, netconf); err != nil {
				return fmt.Errorf("DHCP: cannot configure interface %s: %v", ifname, err)
			}
		}
		if *overrideNetbootURL != "" {
			bootfile = *overrideNetbootURL
		}
		log.Printf("DHCP: boot file for interface %s is %s", ifname, bootfile)
	}
	if *overrideNetbootURL != "" {
		bootfile = *overrideNetbootURL
	}
	debug("DHCP: boot file URL is %s", bootfile)
	// check for supported schemes
	scheme, err := getScheme(bootfile)
	if err != nil {
		return fmt.Errorf("DHCP: cannot get scheme from URL: %v", err)
	}
	if scheme == "" {
		return errors.New("DHCP: no valid scheme found in URL")
	}

	client, err := getClientForBootfile(bootfile)
	if err != nil {
		return fmt.Errorf("DHCP: cannot get client for %s: %v", bootfile, err)
	}
	log.Printf("DHCP: fetching boot file URL: %s", bootfile)

	var resp *http.Response
	for attempt := 0; attempt < maxHTTPAttempts; attempt++ {
		log.Printf("netboot: attempt %d for http.Get", attempt+1)
		req, err := http.NewRequest(http.MethodGet, bootfile, nil)
		if err != nil {
			return fmt.Errorf("could not build request for %s: %v", bootfile, err)
		}
		resp, err = client.Do(req)
		if err != nil && retryableNetError(err) || retryableHTTPError(resp) {
			time.Sleep(retryInterval)
			continue
		}
		if err == nil {
			break
		}
		return fmt.Errorf("DHCP: http.Get of %s failed: %v", bootfile, err)
	}
	// FIXME this will not be called if something fails after this point
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("status code is not 200 OK: %d", resp.StatusCode)
	}
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("DHCP: cannot read boot file from the network: %v", err)
	}
	crypto.TryMeasureData(crypto.BootConfig, body, bootfile)
	u, err := url.Parse(bootfile)
	if err != nil {
		return fmt.Errorf("DHCP: cannot parse URL %s: %v", bootfile, err)
	}
	// extract file name component
	if strings.HasSuffix(u.Path, "/") {
		return fmt.Errorf("invalid file path, cannot end with '/': %s", u.Path)
	}
	filename := filepath.Base(u.Path)
	if filename == "." || filename == "" {
		return fmt.Errorf("invalid empty file name extracted from file path %s", u.Path)
	}
	if err = ioutil.WriteFile(filename, body, 0400); err != nil {
		return fmt.Errorf("DHCP: cannot write to file %s: %v", filename, err)
	}
	debug("DHCP: saved boot file to %s", filename)
	if !*dryRun {
		log.Printf("DHCP: kexec'ing into %s", filename)
		kernel, err := os.OpenFile(filename, os.O_RDONLY, 0)
		if err != nil {
			return fmt.Errorf("DHCP: cannot open file %s: %v", filename, err)
		}
		if err = kexec.FileLoad(kernel, nil /* ramfs */, "" /* cmdline */); err != nil {
			return fmt.Errorf("DHCP: kexec.FileLoad failed: %v", err)
		}
		if err = kexec.Reboot(); err != nil {
			return fmt.Errorf("DHCP: kexec.Reboot failed: %v", err)
		}
	}
	return nil
}

func getScheme(urlstring string) (string, error) {
	u, err := url.Parse(urlstring)
	if err != nil {
		return "", err
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", fmt.Errorf("URL scheme '%s' must be http or https", scheme)
	}
	return scheme, nil
}

func loadCaCerts() (*x509.CertPool, error) {
	rootCAs, err := x509.SystemCertPool()
	if err != nil {
		return nil, err
	}
	if rootCAs == nil {
		debug("certs: rootCAs == nil")
		rootCAs = x509.NewCertPool()
	}
	base64Certs, err := ioutil.ReadFile(*caCertFile)
	if err != nil {
		return nil, fmt.Errorf("could not find cert file '%v' - %v", *caCertFile, err)
	}
	// TODO: Decide if this should also support compressed certs
	// Might be better to have a generic compressed config API
	decLen := base64.StdEncoding.DecodedLen(len(base64Certs))
	certs := make([]byte, decLen)
	n, err := base64.StdEncoding.Decode(certs, base64Certs)
	if err != nil {
		return nil, fmt.Errorf("failed to append %s to RootCAs: %v", *caCertFile, err)
	}
	if ok := rootCAs.AppendCertsFromPEM(certs); !ok {
		debug("No certs extracted from VPD, using system certs only")
	} else {
		debug("%d bytes of certs extrated from VPD", n)
	}
	return rootCAs, nil

}

func getClientForBootfile(bootfile string) (*http.Client, error) {
	var client *http.Client
	scheme, err := getScheme(bootfile)
	if err != nil {
		return nil, err
	}

	switch scheme {
	case "https":
		var config *tls.Config
		if *skipCertVerify {
			config = &tls.Config{
				InsecureSkipVerify: true,
			}
		} else if *caCertFile != "" {
			rootCAs, err := loadCaCerts()
			if err != nil {
				return nil, err
			}
			config = &tls.Config{
				RootCAs: rootCAs,
			}
		}
		tr := &http.Transport{TLSClientConfig: config}
		client = &http.Client{Transport: tr}
		debug("https client setup (use certs from VPD: %t, skipCertVerify %t)",
			*skipCertVerify, *caCertFile != "")
	case "http":
		client = &http.Client{}
		debug("http client setup")
	default:
		return nil, fmt.Errorf("Scheme %s is unsupported", scheme)
	}
	return client, nil
}

type dhcpFunc func(string) (*netboot.NetConf, string, error)

func dhcp6(ifname string) (*netboot.NetConf, string, error) {
	log.Printf("Trying to obtain a DHCPv6 lease on %s", ifname)
	modifiers := []dhcpv6.Modifier{
		dhcpv6.WithArchType(iana.EFI_X86_64),
	}
	if *userClass != "" {
		modifiers = append(modifiers, dhcpv6.WithUserClass([]byte(*userClass)))
	}
	conversation, err := netboot.RequestNetbootv6(ifname, time.Duration(*readTimeout)*time.Second, *dhcpRetries, modifiers...)
	for _, m := range conversation {
		debug(m.Summary())
	}
	if err != nil {
		return nil, "", fmt.Errorf("DHCPv6: netboot request for interface %s failed: %v", ifname, err)
	}
	return netboot.ConversationToNetconf(conversation)
}

func dhcp4(ifname string) (*netboot.NetConf, string, error) {
	log.Printf("Trying to obtain a DHCPv4 lease on %s", ifname)
	var modifiers []dhcpv4.Modifier
	if *userClass != "" {
		modifiers = append(modifiers, dhcpv4.WithUserClass(*userClass, false))
	}
	conversation, err := netboot.RequestNetbootv4(ifname, time.Duration(*readTimeout)*time.Second, *dhcpRetries, modifiers...)
	for _, m := range conversation {
		debug(m.Summary())
	}
	if err != nil {
		return nil, "", fmt.Errorf("DHCPv4: netboot request for interface %s failed: %v", ifname, err)
	}
	return netboot.ConversationToNetconfv4(conversation)
}
