package itr

import (
	"encoding/json"
	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
	"github.com/google/gopacket/pfring"
	"github.com/zededa/go-provision/dataplane/fib"
	"github.com/zededa/go-provision/types"
	"log"
	"math/rand"
	"net"
	"sync/atomic"
	"syscall"
	"time"
)

const SNAPLENGTH = 65536

func StartItrThread(threadName string,
	ring *pfring.Ring,
	killChannel chan bool,
	puntChannel chan []byte) {

	log.Println("Starting thread:", threadName)
	// Kill channel will no longer be needed
	// if we return from this function

	if ring == nil {
		log.Printf("Packet capture setup for interface %s failed\n",
			threadName)
	}

	// create raw socket pair for sending LISP packets out
	fd4, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_RAW, syscall.IPPROTO_UDP)
	//fd4, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_RAW, syscall.IPPROTO_RAW)
	if err != nil {
		log.Printf("Failed creating IPv4 raw socket for %s: %s\n",
			threadName, err)
		return
	}
	err = syscall.SetsockoptInt(fd4, syscall.SOL_SOCKET, syscall.SO_SNDBUF, 65536)
	if err != nil {
		log.Printf("Thread %s: Setting socket buffer size failed: %s\n",
			threadName, err)
	}
	defer syscall.Close(fd4)

	//*****
	err = syscall.SetsockoptInt(fd4, syscall.SOL_SOCKET,
		syscall.IP_MTU_DISCOVER, syscall.IP_PMTUDISC_DONT)
	//err = syscall.SetsockoptInt(fd4, syscall.SOL_SOCKET, syscall.IP_MTU_DISCOVER, 5)
	if err != nil {
		log.Printf("Disabling path MTU discovery failed: %s.\n", err)
	}
	err = syscall.SetsockoptInt(fd4, syscall.IPPROTO_IP, syscall.IP_HDRINCL, 0)
	if err != nil {
		log.Printf("Disabling IP_HDRINCL failed: %s.\n", err)
	}
	//*****

	fd6, err := syscall.Socket(syscall.AF_INET6, syscall.SOCK_RAW, syscall.IPPROTO_RAW)
	if err != nil {
		log.Printf("Failed creating IPv6 raw socket for %s: %s\n",
			threadName, err)
		return
	}
	err = syscall.SetsockoptInt(fd6, syscall.SOL_SOCKET, syscall.SO_SNDBUF, 65536)
	if err != nil {
		log.Printf("Thread %s: Setting socket buffer size failed: %s\n", threadName, err)
	}
	defer syscall.Close(fd6)

	rand.Seed(time.Now().UnixNano())
	ivHigh := rand.Uint64()
	ivLow := rand.Uint64()

	itrLocalData := new(types.ITRLocalData)
	itrLocalData.Fd4 = fd4
	itrLocalData.Fd6 = fd6
	itrLocalData.IvHigh = ivHigh
	itrLocalData.IvLow = ivLow

	startWorking(threadName, ring, killChannel, puntChannel,
		itrLocalData)

	// If startWorking returns, it means the control thread wants
	// this thread to die.
	return
}

// Opens up the pfing on interface and sets up packet capture.
func SetupPacketCapture(ifname string, snapLen uint32) *pfring.Ring {
	// create a new pf_ring to capture packets from our interface
	ring, err := pfring.NewRing(ifname, SNAPLENGTH, pfring.FlagPromisc)
	if err != nil {
		log.Printf("PF_RING creation for interface %s failed: %s\n",
			ifname, err)
		return nil
	}

	// Capture ipv6 packets only
	err = ring.SetBPFFilter("ip6")
	if err != nil {
		log.Print("Setting ipv6 BPF filter on interface %s failed: %s\n",
			ifname, err)
		ring.Close()
		return nil
	}

	// Make PF_RING capture only transmitted packet
	ring.SetDirection(pfring.TransmitOnly)

	// set the ring in readonly mode
	ring.SetSocketMode(pfring.ReadOnly)

	ring.SetPollWatermark(1)
	// set a poll duration of 1 hour
	ring.SetPollDuration(60 * 60 * 1000)

	// Enable ring. Packet inflow starts after this.
	err = ring.Enable()
	if err != nil {
		log.Printf("Failed enabling PF_RING for interface %s: %s\n",
			ifname, err)
		ring.Close()
		return nil
	}
	return ring
}

// Start capturing and processing packets.
func startWorking(ifname string, ring *pfring.Ring,
	killChannel chan bool, puntChannel chan []byte,
	itrLocalData *types.ITRLocalData) {
	var pktBuf [SNAPLENGTH]byte

	iid := fib.LookupIfaceIID(ifname)
	if iid == 0 {
		log.Printf("Interface %s's IID cannot be found\n", ifname)
		return
	}

	// We need the EIDs attached to this interfaces for further processing
	// Keep looking for them every 100ms
	var eids []net.IP
eidLoop:
	for {
		time.Sleep(2 * time.Second)
		select {
		case <-killChannel:
			log.Printf("ITR thread %s received terminate from control module.", ifname)
			return
		default:
			// EID map database might not have come yet. Wait for before we start
			// processing packets.
			eids = fib.LookupIfaceEids(iid)
			if eids != nil {
				break eidLoop
			}
			log.Println("Re-trying EID lookup for interface", ifname)
			continue
		}
	}

	/*
	 * While waiting for packets we should also look for the terminate
	 * message from control module. If control module sends a terminate,
	 * ITR thread should free all its allocated resources, stop processing
	 * packets and exit.
	 */
	for {
		select {
		case <-killChannel:
			// Channel becomes readable when it's closed.
			// So we terminate the thread either when we see "true" coming in it or
			// when the control thread closes our communication channel.
			log.Printf("ITR thread %s received terminate from control module.", ifname)
			return
		default:
			ci, err := ring.ReadPacketDataTo(pktBuf[types.MAXHEADERLEN:])
			if err != nil {
				log.Printf(
					"Something wrong with packet capture from interface %s: %s\n",
					ifname, err)
				log.Printf(
					"May be we are asked to terminate after the hosting domU died.\n")
				return
			}

			pktLen := ci.CaptureLength
			if pktLen <= 0 {
				// XXX May be add a per thread stat here
				continue
			}
			packet := gopacket.NewPacket(
				pktBuf[types.MAXHEADERLEN:ci.CaptureLength+types.MAXHEADERLEN],
				layers.LinkTypeEthernet,
				//gopacket.DecodeOptions{Lazy: true, NoCopy: true})
				gopacket.DecodeOptions{Lazy: false, NoCopy: true})
			ip6Layer := packet.Layer(layers.LayerTypeIPv6)
			if ip6Layer == nil {
				// XXX May be add a per thread stat here
				// Ignore this packet.
				continue
			}

			ipHeader := ip6Layer.(*layers.IPv6)

			// Check if the source address of packet matches with any of the eids
			// assigned to input interface.
			srcAddr := ipHeader.SrcIP
			matchFound := false
			for _, eid := range eids {
				if srcAddr.Equal(eid) == true {
					matchFound = true
					break
				}
			}

			if !matchFound {
				// XXX May be add a per thread stat here
				log.Printf(
					"Thread: %s: Input packet with source address %s does not have matching EID of interface\n",
					ifname, srcAddr)
				continue
			}

			dstAddr := ipHeader.DstIP

			/**
			 * Compute hash of packet.
			 * LSB 4 bytes of src addr (xor) LSB 4 bytes of dst addr (xor)
			 * (src port << 16 | dst port)
			 */
			var srcAddrBytes uint32 = (uint32(srcAddr[12])<<24 |
				uint32(srcAddr[13])<<16 |
				uint32(srcAddr[14])<<8 | uint32(srcAddr[15]))
			var dstAddrBytes uint32 = (uint32(dstAddr[12])<<24 |
				uint32(dstAddr[13])<<16 |
				uint32(dstAddr[14])<<8 | uint32(dstAddr[15]))
			transportLayer := packet.TransportLayer()

			var ports uint32 = 0
			if (ipHeader.NextHeader == layers.IPProtocolUDP) ||
				(ipHeader.NextHeader == layers.IPProtocolTCP) {
				// This is a byte array of the header
				transportContents := transportLayer.LayerContents()

				// XXX What do we do when there is no transport header? like PING
				if transportContents != nil {
					//log.Println("XXXXX Transport contents:", transportContents)
					ports = (uint32(transportContents[0])<<24 |
						uint32(transportContents[1])<<16 |
						uint32(transportContents[2])<<8 |
						uint32(transportContents[3]))
				}
			}

			var hash32 uint32 = srcAddrBytes ^ dstAddrBytes ^ ports

			log.Printf("XXXXX Packet capture length is %d\n", pktLen)
			LookupAndSend(packet, pktBuf[:],
				uint32(pktLen), ci.Timestamp, iid, hash32,
				ifname, srcAddr, dstAddr,
				puntChannel, itrLocalData)
		}
	}
}

// This function expects the parameter pktBuf to be a statically
// allocated buffer longer than the original packet length.
// We currently use a buffer of length 64K bytes.
//
// Perform lookup into mapcache database and forward if the lookup succeeds.
// If not, buffer the packet and send a punt request to lispers.net for resolution.
// Look for comments inside the function to understand more about what it does.
func LookupAndSend(packet gopacket.Packet,
	pktBuf []byte,
	capLen uint32,
	timeStamp time.Time,
	iid uint32,
	hash32 uint32,
	ifname string,
	srcAddr net.IP,
	dstAddr net.IP,
	puntChannel chan []byte,
	itrLocalData *types.ITRLocalData) {

	// Look for the map-cache entry required
	mapEntry, punt := fib.LookupAndAdd(iid, dstAddr, timeStamp)

	if mapEntry.Resolved != true {
		// Buffer the packet for processing later

		// Add packet to channel in a non blocking fashion.
		// Buffered packet channel is only 10 entries long.
		select {
		case mapEntry.PktBuffer <- &types.BufferedPacket{
			Packet: packet,
			Hash32: hash32,
		}:
			atomic.AddUint64(&mapEntry.BuffdPkts, 1)
		default:
			log.Println("Packet buffer channel full for EID", dstAddr)
			atomic.AddUint64(&mapEntry.TailDrops, 1)
		}

		/**
		 * There is no guarantee that the control thread has not
		 * resolved our unresolved map entry by the time we add packet
		 * to buffered packet channel. We perform the lookup for our
		 * iid, eid once again with read lock and check the resolution
		 * status.
		 *
		 * If the map cache entry is resolved by now, we dequeue one
		 * packet from the buffered packet channel and send it out.
		 * This avoids the case where control thread has already sent
		 * out all buffered packets and our packet sits in the buffered
		 * channel without being noticed.
		 */
		mapEntry, punt1 := fib.LookupAndAdd(iid, dstAddr, timeStamp)
		if mapEntry.Resolved {
			punt = punt1
			select {
			case pkt := <-mapEntry.PktBuffer:
				// Packet read into pktBuf buffer might have changed.
				// It is not safe to pass it's pointer.
				// Extract the packet data from buffered packet
				pktBytes := pkt.Packet.Data()
				capLen = uint32(len(pktBytes))

				// copy packet bytes into pktBuf at an offset of MAXHEADERLEN bytes
				// ipv6 (40) + UDP (8) + LISP (8) - ETHERNET (14) + LISP IV (16) = 58
				copy(pktBuf[types.MAXHEADERLEN:], pktBytes)

				// Encapsulate and send packet out
				fib.CraftAndSendLispPacket(pkt.Packet, pktBuf, capLen, timeStamp,
					pkt.Hash32, mapEntry, iid, itrLocalData)

				// look golang atomic increment documentation to understand ^uint64(0)
				// We are trying to decrement the counter here by 1
				atomic.AddUint64(&mapEntry.BuffdPkts, ^uint64(0))

				// Increment packet, byte counts
				atomic.AddUint64(&mapEntry.Packets, 1)
				atomic.AddUint64(&mapEntry.Bytes, uint64(capLen))
			default:
				// We do not want to get blocked and keep waiting
				// when there are no packets in the buffer channel.
			}
		} else {
			// Look for the default route
			defaultPrefix := net.ParseIP("::")
			defaultMap, _ := fib.LookupAndAdd(iid, defaultPrefix, timeStamp)
			if defaultMap.Resolved {
				fib.CraftAndSendLispPacket(packet, pktBuf, capLen, timeStamp,
					hash32, defaultMap, iid, itrLocalData)
			}
		}
	} else {
		// Craft the LISP header, outer layers here and send packet out
		fib.CraftAndSendLispPacket(packet, pktBuf, capLen, timeStamp,
			hash32, mapEntry, iid, itrLocalData)
		atomic.AddUint64(&mapEntry.Packets, 1)
		atomic.AddUint64(&mapEntry.Bytes, uint64(capLen))
	}
	if punt == true {
		// We will have to put a punt request on the control
		// module's channel
		puntEntry := types.PuntEntry{
			Type:  "discovery",
			Deid:  dstAddr,
			Seid:  srcAddr,
			Iface: ifname,
		}
		puntMsg, err := json.Marshal(puntEntry)
		if err != nil {
			log.Printf("Marshaling punt entry failed %s: %s\n",
				puntEntry, err)
		} else {
			puntChannel <- puntMsg
			log.Println("Sending punt entry at", time.Now(), ":", string(puntMsg))
		}
	}
	return
}
