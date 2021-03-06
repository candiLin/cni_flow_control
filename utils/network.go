package utils

import (
	"fmt"
	log "github.com/Sirupsen/logrus"
	"github.com/containernetworking/cni/pkg/ip"
	"github.com/containernetworking/cni/pkg/ns"
	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/cni/pkg/types/current"
	"github.com/vishvananda/netlink"
	"io"
	"net"
	"os"
        "reflect"
	"syscall"
	"strconv"
)

// DoNetworking performs the networking for the given config and IPAM result
func DoNetworking(args *skel.CmdArgs, conf NetConf, result *current.Result, logger *log.Entry, desiredVethName string, ingress_bandwidth string, egress_bandwidth string) (hostVethName, contVethMAC string, err error) {
	// Select the first 11 characters of the containerID for the host veth.
	hostVethName = "cali" + args.ContainerID[:Min(11, len(args.ContainerID))]
	ifbname := "ifb" + args.ContainerID[:Min(11, len(args.ContainerID))]
	contVethName := args.IfName
	var hasIPv4, hasIPv6 bool

	// If a desired veth name was passed in, use that instead.
	if desiredVethName != "" {
		hostVethName = desiredVethName
	}

	// Clean up if hostVeth exists.
	if oldHostVeth, err := netlink.LinkByName(hostVethName); err == nil {
		if err = netlink.LinkDel(oldHostVeth); err != nil {
			return "", "", fmt.Errorf("failed to delete old hostVeth %v: %v", hostVethName, err)
		}
		logger.Infof("clean old hostVeth: %v", hostVethName)
	}

	err = ns.WithNetNSPath(args.Netns, func(hostNS ns.NetNS) error {
		veth := &netlink.Veth{
			LinkAttrs: netlink.LinkAttrs{
				Name:  contVethName,
				Flags: net.FlagUp,
				MTU:   conf.MTU,
				TxQLen: 1000,
			},
			PeerName: hostVethName,
		}

		if err := netlink.LinkAdd(veth); err != nil {
			logger.Errorf("Error adding veth %+v: %s", veth, err)
			return err
		}

		hostVeth, err := netlink.LinkByName(hostVethName)
		if err != nil {
			err = fmt.Errorf("failed to lookup %q: %v", hostVethName, err)
			return err
		}

		// Explicitly set the veth to UP state, because netlink doesn't always do that on all the platforms with net.FlagUp.
		// veth won't get a link local address unless it's set to UP state.
		if err = netlink.LinkSetUp(hostVeth); err != nil {
			return fmt.Errorf("failed to set %q up: %v", hostVethName, err)
		}

		contVeth, err := netlink.LinkByName(contVethName)
		if err != nil {
			err = fmt.Errorf("failed to lookup %q: %v", contVethName, err)
			return err
		}

		// Fetch the MAC from the container Veth. This is needed by Calico.
		contVethMAC = contVeth.Attrs().HardwareAddr.String()
		logger.WithField("MAC", contVethMAC).Debug("Found MAC for container veth")

		// At this point, the virtual ethernet pair has been created, and both ends have the right names.
		// Both ends of the veth are still in the container's network namespace.

		for _, addr := range result.IPs {

			// Before returning, create the routes inside the namespace, first for IPv4 then IPv6.
			if addr.Version == "4" {
				// Add a connected route to a dummy next hop so that a default route can be set
				gw := net.IPv4(169, 254, 1, 1)
				gwNet := &net.IPNet{IP: gw, Mask: net.CIDRMask(32, 32)}
				if err = netlink.RouteAdd(&netlink.Route{
					LinkIndex: contVeth.Attrs().Index,
					Scope:     netlink.SCOPE_LINK,
					Dst:       gwNet}); err != nil {
					return fmt.Errorf("failed to add route %v", err)
				}

				if err = ip.AddDefaultRoute(gw, contVeth); err != nil {
					return fmt.Errorf("failed to add route %v", err)
				}

				if err = netlink.AddrAdd(contVeth, &netlink.Addr{IPNet: &addr.Address}); err != nil {
					return fmt.Errorf("failed to add IP addr to %q: %v", contVethName, err)
				}
				// Set hasIPv4 to true so sysctls for IPv4 can be programmed when the host side of
				// the veth finishes moving to the host namespace.
				hasIPv4 = true
			}

			// Handle IPv6 routes
			if addr.Version == "6" {
				// No need to add a dummy next hop route as the host veth device will already have an IPv6
				// link local address that can be used as a next hop.
				// Just fetch the address of the host end of the veth and use it as the next hop.
				addresses, err := netlink.AddrList(hostVeth, netlink.FAMILY_V6)
				if err != nil {
					logger.Errorf("Error listing IPv6 addresses: %s", err)
					return err
				}

				if len(addresses) < 1 {
					// If the hostVeth doesn't have an IPv6 address then this host probably doesn't
					// support IPv6. Since a IPv6 address has been allocated that can't be used,
					// return an error.
					return fmt.Errorf("failed to get IPv6 addresses for host side of the veth pair")
				}

				hostIPv6Addr := addresses[0].IP

				_, defNet, _ := net.ParseCIDR("::/0")
				if err = ip.AddRoute(defNet, hostIPv6Addr, contVeth); err != nil {
					return fmt.Errorf("failed to add default gateway to %v %v", hostIPv6Addr, err)
				}

				if err = netlink.AddrAdd(contVeth, &netlink.Addr{IPNet: &addr.Address}); err != nil {
					return fmt.Errorf("failed to add IP addr to %q: %v", contVeth, err)
				}

				// Set hasIPv6 to true so sysctls for IPv6 can be programmed when the host side of
				// the veth finishes moving to the host namespace.
				hasIPv6 = true
			}
		}

		// Now that the everything has been successfully set up in the container, move the "host" end of the
		// veth into the host namespace.
		if err = netlink.LinkSetNsFd(hostVeth, int(hostNS.Fd())); err != nil {
			return fmt.Errorf("failed to move veth to host netns: %v", err)
		}

		return nil
	})

	if err != nil {
		logger.Errorf("Error creating veth: %s", err)
		return "", "", err
	}

	err = configureSysctls(hostVethName, hasIPv4, hasIPv6)
	if err != nil {
		return "", "", fmt.Errorf("error configuring sysctls for interface: %s, error: %s", hostVethName, err)
	}

	// Moving a veth between namespaces always leaves it in the "DOWN" state. Set it back to "UP" now that we're
	// back in the host namespace.
	hostVeth, err := netlink.LinkByName(hostVethName)
	if err != nil {
		return "", "", fmt.Errorf("failed to lookup %q: %v", hostVethName, err)
	}

	if err = netlink.LinkSetUp(hostVeth); err != nil {
		return "", "", fmt.Errorf("failed to set %q up: %v", hostVethName, err)
	}
        
	// Now that the host side of the veth is moved, state set to UP, and configured with sysctls, we can add the routes to it in the host namespace.
	err = setupRoutes(hostVeth, result)
	if err != nil {
		return "", "", fmt.Errorf("error adding host side routes for interface: %s, error: %s", hostVeth.Attrs().Name, err)
	
	
     }
              Rate1, err := strconv.Atoi(ingress_bandwidth)
                if err != nil {
			fmt.Println("convert fail")
			      }
		logger.Infof("lj speed: %s", Rate1)
		Rate2, err := strconv.Atoi(egress_bandwidth)
		if err != nil{
			fmt.Println("convert fail")
					}  
	        logger.Infof("lj speed: %s", Rate2)
         	index := hostVeth.Attrs().Index
		qdiscHandle := netlink.MakeHandle(0x2, 0x0)
		qdiscAttrs := netlink.QdiscAttrs{
			LinkIndex: index,
			Handle:    qdiscHandle,
			Parent:    netlink.HANDLE_ROOT,
		}
		qdisc := netlink.NewHtb(qdiscAttrs)
		if err := netlink.QdiscAdd(qdisc); err != nil {
			fmt.Println("add qdisc err")
		}
		qdiscs, err := netlink.QdiscList(hostVeth)
		if err != nil {
			fmt.Println("list qdisc err")
		}
		if len(qdiscs) != 1 {
			fmt.Println("Failed to add qdisc")
		}
		_, ok := qdiscs[0].(*netlink.Htb)
		if !ok {
			fmt.Println("Qdisc is the wrong type")
		}

		classId := netlink.MakeHandle(0x2, 0x56cb)
		classAttrs := netlink.ClassAttrs{
			LinkIndex: index,
			Parent:    qdiscHandle,
			Handle:    classId,
		}
		htbClassAttrs := netlink.HtbClassAttrs{
			Rate:   uint64(Rate1),
			Buffer: 32*100000,
		}
		htbClass := netlink.NewHtbClass(classAttrs, htbClassAttrs)
		if err = netlink.ClassReplace(htbClass); err != nil {
			fmt.Println("Failed to add a HTB class: %v", err)
		}
		classes, err := netlink.ClassList(hostVeth, qdiscHandle)
		if err != nil {
			fmt.Println("list class err")
		}
		if len(classes) != 1 {
			fmt.Println("Failed to add class")
			fmt.Println("length of classes is : %v", len(classes))
		}
		_, ok = classes[0].(*netlink.HtbClass)
		if !ok {
			fmt.Println("Class is the wrong type")
		}
		u32SelKeys := []netlink.TcU32Key{

			netlink.TcU32Key{
				Mask:    0x00000000,
				Val:     0x00000000,
				Off:     16,
				OffMask: 0,
			},
		}
		filter := &netlink.U32{
			FilterAttrs: netlink.FilterAttrs{
				LinkIndex: index,
				Parent:    qdiscHandle,
				Priority:  1,
				Protocol:  syscall.ETH_P_IP,
			},
			Sel: &netlink.TcU32Sel{
				Keys:  u32SelKeys,
				Flags: netlink.TC_U32_TERMINAL,
			},
			ClassId: classId,
			Actions: []netlink.Action{},
		}

		cFilter := *filter
		if err := netlink.FilterAdd(filter); err != nil {
			fmt.Println("add filter err")
		}
		if !reflect.DeepEqual(cFilter, *filter) {
			fmt.Println("U32 %v and %v are not equal", cFilter, *filter)
		}

		filters, err := netlink.FilterList(hostVeth, qdiscHandle)
		if err != nil {
			fmt.Println("filter list err")
		}
		if len(filters) != 1 {
			fmt.Println("Failed to add filter")
		}
         if err := netlink.LinkAdd(&netlink.Ifb{netlink.LinkAttrs{Name: ifbname, TxQLen: 1000}}); err != nil {
		fmt.Println("create ifb wrong")
	}
	redir, _ := netlink.LinkByName(ifbname)
	if err := netlink.LinkSetUp(redir); err != nil {
		fmt.Println("set up foo err")
	}
	qdisc_ingress := &netlink.Ingress{
		QdiscAttrs: netlink.QdiscAttrs{
			LinkIndex: hostVeth.Attrs().Index,
			Handle:    netlink.MakeHandle(0xffff, 0),
			Parent:    netlink.HANDLE_INGRESS,
		},
	}
	if err := netlink.QdiscAdd(qdisc_ingress); err != nil {
		fmt.Println("add qdisc err")
	}
	classId_ingress := netlink.MakeHandle(1, 1)
	filter_ingress := &netlink.U32{
		FilterAttrs: netlink.FilterAttrs{
			LinkIndex: hostVeth.Attrs().Index,
			Parent:    netlink.MakeHandle(0xffff, 0),
			Priority:  1,
			Protocol:  syscall.ETH_P_IP,
		},
		RedirIndex: redir.Attrs().Index,
		ClassId:    classId_ingress,
	}
	if err := netlink.FilterAdd(filter_ingress); err != nil {
		fmt.Println("add filter err")
	}
	index_ingress := redir.Attrs().Index

	qdiscHandle_ingress := netlink.MakeHandle(0x1, 0x0)
	qdiscAttrs_ingress := netlink.QdiscAttrs{
		LinkIndex: index_ingress,
		Handle:    qdiscHandle_ingress,
		Parent:    netlink.HANDLE_ROOT,
	}

	qdisc_ingress_2 := netlink.NewHtb(qdiscAttrs_ingress)
	if err := netlink.QdiscAdd(qdisc_ingress_2); err != nil {
		fmt.Println("add qdisc err")
	}

	classId_ingress_2 := netlink.MakeHandle(0x1, 0x56cb)
	classAttrs_ingress := netlink.ClassAttrs{
		LinkIndex: index_ingress,
		Parent:    qdiscHandle_ingress,
		Handle:    classId_ingress_2,
	}
	htbClassAttrs_ingress := netlink.HtbClassAttrs{
		Rate:   uint64(Rate2),
		Buffer: 32 * 1024,
	}
	htbClass_ingress := netlink.NewHtbClass(classAttrs_ingress, htbClassAttrs_ingress)
	if err := netlink.ClassReplace(htbClass_ingress); err != nil {
		fmt.Println("Failed to add a HTB class: %v", err)
	}

	u32SelKeys_ingress := []netlink.TcU32Key{

		netlink.TcU32Key{
			Mask:    0x00000000,
			Val:     0x00000000,
			Off:     12,
			OffMask: 0,
		},
	}
	filter_ingress_2 := &netlink.U32{
		FilterAttrs: netlink.FilterAttrs{
			LinkIndex: index_ingress,
			Parent:    qdiscHandle_ingress,
			Priority:  1,
			Protocol:  syscall.ETH_P_IP,
		},
		Sel: &netlink.TcU32Sel{
			Keys:  u32SelKeys_ingress,
			Flags: netlink.TC_U32_TERMINAL,
		},
		ClassId: classId_ingress_2,
		Actions: []netlink.Action{},
	}

	if err := netlink.FilterAdd(filter_ingress_2); err != nil {
		fmt.Println("add filter err")
	}
	return hostVethName, contVethMAC, err
}

// setupRoutes sets up the routes for the host side of the veth pair.
func setupRoutes(hostVeth netlink.Link, result *current.Result) error {
	for _, ip := range result.IPs {
		err := netlink.RouteAdd(
			&netlink.Route{
				LinkIndex: hostVeth.Attrs().Index,
				Scope:     netlink.SCOPE_LINK,
				Dst:       &ip.Address,
			})
			if err != nil {
			return fmt.Errorf("failed to add route %v", err)
		}

		log.Debugf("CNI adding route for interface: %v, IP: %s", hostVeth, ip.Address)
	}
	return nil
}

// configureSysctls configures necessary sysctls required for the host side of the veth pair for IPv4 and/or IPv6.
func configureSysctls(hostVethName string, hasIPv4, hasIPv6 bool) error {
	var err error

	if hasIPv4 {
		// Enable proxy ARP, this makes the host respond to all ARP requests with its own
		// MAC. We install explicit routes into the containers network
		// namespace and we use a link-local address for the gateway.  Turing on proxy ARP
		// means that we don't need to assign the link local address explicitly to each
		// host side of the veth, which is one fewer thing to maintain and one fewer
		// thing we may clash over.
		if err = writeProcSys(fmt.Sprintf("/proc/sys/net/ipv4/conf/%s/proxy_arp", hostVethName), "1"); err != nil {
			return err
		}

		// Normally, the kernel has a delay before responding to proxy ARP but we know
		// that's not needed in a Calico network so we disable it.
		if err = writeProcSys(fmt.Sprintf("/proc/sys/net/ipv4/neigh/%s/proxy_delay", hostVethName), "0"); err != nil {
			return err
		}

		// Enable IP forwarding of packets coming _from_ this interface.  For packets to
		// be forwarded in both directions we need this flag to be set on the fabric-facing
		// interface too (or for the global default to be set).
		if err = writeProcSys(fmt.Sprintf("/proc/sys/net/ipv4/conf/%s/forwarding", hostVethName), "1"); err != nil {
			return err
		}
	}

	if hasIPv6 {
		// Enable proxy NDP, similarly to proxy ARP, described above in IPv4 section.
		if err = writeProcSys(fmt.Sprintf("/proc/sys/net/ipv6/conf/%s/proxy_ndp", hostVethName), "1"); err != nil {
			return err
		}

		// Enable IP forwarding of packets coming _from_ this interface.  For packets to
		// be forwarded in both directions we need this flag to be set on the fabric-facing
		// interface too (or for the global default to be set).
		if err = writeProcSys(fmt.Sprintf("/proc/sys/net/ipv6/conf/%s/forwarding", hostVethName), "1"); err != nil {
			return err
		}
	}

	return nil
}

// writeProcSys takes the sysctl path and a string value to set i.e. "0" or "1" and sets the sysctl.
func writeProcSys(path, value string) error {
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	n, err := f.Write([]byte(value))
	if err == nil && n < len(value) {
		err = io.ErrShortWrite
	}
	if err1 := f.Close(); err == nil {
		err = err1
	}
	return err
}
