package modules

import (
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/evilsocket/bettercap-ng/core"
	"github.com/evilsocket/bettercap-ng/log"
	"github.com/evilsocket/bettercap-ng/net"
	"github.com/evilsocket/bettercap-ng/packets"
	"github.com/evilsocket/bettercap-ng/session"

	"github.com/dustin/go-humanize"
	"github.com/olekukonko/tablewriter"
)

type Discovery struct {
	session.SessionModule

	refresh int
	before  net.ArpTable
	current net.ArpTable
	quit    chan bool
}

func NewDiscovery(s *session.Session) *Discovery {
	d := &Discovery{
		SessionModule: session.NewSessionModule("net.recon", s),

		refresh: 1,
		before:  nil,
		current: nil,
		quit:    make(chan bool),
	}

	d.AddHandler(session.NewModuleHandler("net.recon on", "",
		"Start network hosts discovery.",
		func(args []string) error {
			return d.Start()
		}))

	d.AddHandler(session.NewModuleHandler("net.recon off", "",
		"Stop network hosts discovery.",
		func(args []string) error {
			return d.Stop()
		}))

	d.AddHandler(session.NewModuleHandler("net.show", "",
		"Show current hosts list.",
		func(args []string) error {
			return d.Show()
		}))

	return d
}

func (d Discovery) Name() string {
	return "net.recon"
}

func (d Discovery) Description() string {
	return "Read periodically the ARP cache in order to monitor for new hosts on the network."
}

func (d Discovery) Author() string {
	return "Simone Margaritelli <evilsocket@protonmail.com>"
}

func (d *Discovery) checkShared(new net.ArpTable) {
	n_gw_shared := 0
	for ip, mac := range new {
		if ip != d.Session.Gateway.IpAddress && mac == d.Session.Gateway.HwAddress {
			n_gw_shared++
		}
	}

	if n_gw_shared > 0 {
		a := ""
		b := ""
		if n_gw_shared == 1 {
			a = ""
			b = "s"
		} else {
			a = "s"
			b = ""
		}

		log.Warning("Found %d endpoint%s which share%s the same MAC of the gateway (%s), there're might be some IP isolation going on, skipping.", n_gw_shared, a, b, d.Session.Gateway.HwAddress)
	}
}

func (d *Discovery) runDiff() {
	var new net.ArpTable = make(net.ArpTable)
	var rem net.ArpTable = make(net.ArpTable)

	if d.before != nil {
		new = net.ArpDiff(d.current, d.before)
		rem = net.ArpDiff(d.before, d.current)
	} else {
		new = d.current
	}

	if len(new) > 0 || len(rem) > 0 {
		d.checkShared(new)

		// refresh target pool
		for ip, mac := range rem {
			d.Session.Targets.Remove(ip, mac)
		}

		for ip, mac := range new {
			d.Session.Targets.AddIfNotExist(ip, mac)
		}
	}
}

func (d *Discovery) Configure() error {
	return nil
}

func (d *Discovery) Start() error {
	if d.Running() == true {
		return session.ErrAlreadyStarted
	} else if err := d.Configure(); err != nil {
		return err
	}

	d.SetRunning(true)

	go func() {
		for {
			select {
			case <-time.After(time.Duration(d.refresh) * time.Second):
				var err error

				if d.current, err = net.ArpUpdate(d.Session.Interface.Name()); err != nil {
					log.Error("%s", err)
					continue
				}

				d.runDiff()

				d.before = d.current

			case <-d.quit:
				return
			}
		}
	}()

	return nil
}

type tSorter []*net.Endpoint

func (a tSorter) Len() int           { return len(a) }
func (a tSorter) Swap(i, j int)      { a[i], a[j] = a[j], a[i] }
func (a tSorter) Less(i, j int) bool { return a[i].IpAddressUint32 < a[j].IpAddressUint32 }

func (d *Discovery) Show() error {
	d.Session.Targets.Lock()
	defer d.Session.Targets.Unlock()

	iface := d.Session.Interface
	gw := d.Session.Gateway

	data := [][]string{
		[]string{core.Green("interface"), core.Bold(iface.Name()), iface.IpAddress, iface.HwAddress, core.Dim(iface.Vendor)},
		[]string{core.Green("gateway"), core.Bold(gw.Hostname), gw.IpAddress, gw.HwAddress, core.Dim(gw.Vendor)},
	}

	table := tablewriter.NewWriter(os.Stdout)

	table.AppendBulk(data)
	table.Render()

	fmt.Println()

	nTargets := len(d.Session.Targets.Targets)
	if nTargets == 0 {
		fmt.Println(core.Dim("No endpoints discovered so far."))
	} else {
		targets := make([]*net.Endpoint, 0, nTargets)
		for _, t := range d.Session.Targets.Targets {
			targets = append(targets, t)
		}

		sort.Sort(tSorter(targets))

		data = make([][]string, nTargets)
		for i, t := range targets {
			var traffic *packets.Traffic
			var found bool

			if traffic, found = d.Session.Queue.Traffic[t.IpAddress]; found == false {
				traffic = &packets.Traffic{}
			}

			data[i] = []string{
				t.IpAddress,
				t.HwAddress,
				core.Yellow(t.Hostname),
				t.Vendor,
				humanize.Bytes(traffic.Sent),
				humanize.Bytes(traffic.Received),
				t.LastSeen.Format("2006-01-02 15:04:05"),
			}
		}

		table = tablewriter.NewWriter(os.Stdout)

		table.SetHeader([]string{"IP", "MAC", "Hostname", "Vendor", "Sent", "Recvd", "Last Seen"})
		table.AppendBulk(data)
		table.Render()

		fmt.Println()
	}

	row := []string{
		humanize.Bytes(d.Session.Queue.Sent),
		humanize.Bytes(d.Session.Queue.Received),
		fmt.Sprintf("%d", d.Session.Queue.PktReceived),
		fmt.Sprintf("%d", d.Session.Queue.Errors),
	}

	table = tablewriter.NewWriter(os.Stdout)

	table.SetHeader([]string{"Sent", "Sniffed", "# Packets", "Errors"})
	table.Append(row)
	table.Render()

	fmt.Println()

	table = tablewriter.NewWriter(os.Stdout)

	for proto, hits := range d.Session.Queue.Protos {
		table.Append([]string{proto, fmt.Sprintf("%d", hits)})
	}

	table.SetHeader([]string{"Proto", "# Packets"})
	table.Render()

	return nil
}

func (d *Discovery) Stop() error {
	if d.Running() == false {
		return session.ErrAlreadyStopped
	}
	d.quit <- true
	d.SetRunning(false)
	return nil
}
