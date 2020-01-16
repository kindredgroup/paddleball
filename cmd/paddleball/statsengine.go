package main

//
// Copyright (c) 2019 Tony Sarendal <tony@polarcap.org>
//
// Permission to use, copy, modify, and distribute this software for any
// purpose with or without fee is hereby granted, provided that the above
// copyright notice and this permission notice appear in all copies.
//
// THE SOFTWARE IS PROVIDED "AS IS" AND THE AUTHOR DISCLAIMS ALL WARRANTIES
// WITH REGARD TO THIS SOFTWARE INCLUDING ALL IMPLIED WARRANTIES OF
// MERCHANTABILITY AND FITNESS. IN NO EVENT SHALL THE AUTHOR BE LIABLE FOR
// ANY SPECIAL, DIRECT, INDIRECT, OR CONSEQUENTIAL DAMAGES OR ANY DAMAGES
// WHATSOEVER RESULTING FROM LOSS OF USE, DATA OR PROFITS, WHETHER IN AN
// ACTION OF CONTRACT, NEGLIGENCE OR OTHER TORTIOUS ACTION, ARISING OUT OF
// OR IN CONNECTION WITH THE USE OR PERFORMANCE OF THIS SOFTWARE.
//

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

type packetStats struct {
	dropPkts, dupPkts, reordPkts, rcvdPkts int64
	pbdropPkts                             int64
	minRtt, maxRtt, totRtt                 time.Duration
}

type jsonReport struct {
	Tag                   string
	TimestampUtc          time.Time
	ReceivedPackets       int64
	DroppedPackets        int64
	DuplicatePackets      int64
	ReorderedPackets      int64
	AverageRTT            float64
	LowestRTT             float64
	HighestRTT            float64
	PBQueueDroppedPackets int
	PBQueueLength         int
	PBQueueCapacity       int
}

func statsEngine(rp <-chan payload, global *packetStats, printJson string) {
	serialNumbers := make(map[int64]int64) // the expected serial number for each id
	workWindow := []payload{}              // packets to analyze
	feedWindow := []payload{}              // insert packets

	ticker := time.NewTicker(time.Second)
	message := payload{}

	for {
		select {
		case message = <-rp:
			feedWindow = append(feedWindow, message)
		case <-ticker.C:
			local := process(workWindow, feedWindow, serialNumbers)
			statsUpdate(global, local)

			workWindow = feedWindow  // change feed to work
			feedWindow = []payload{} // re-init feed

			statsPrint(&local, printJson, len(rp), cap(rp))
		}
	}
}

func process(workWindow []payload, feedWindow []payload, serialNumbers map[int64]int64) packetStats {
	local := packetStats{}

	// Check workWindow for the lowest serial numbers for each Id.
	// Update the expected serial numbers and return the number
	// of missing packets.
	local.dropPkts = fastForward(serialNumbers, workWindow)

	// Process the workWindow packet by packet.
	for position, message := range workWindow {
		local.pbdropPkts = local.pbdropPkts + message.Pbdrop
		updateRtt(message, &local)

		_, ok := serialNumbers[message.Id]
		if !ok { // Initial packet from this sender ID.
			serialNumbers[message.Id] = message.Serial + 1
			local.rcvdPkts++
			continue
		}

		// Lower serial than expected. Already calculated as drop/dup/re-order.
		if message.Serial < serialNumbers[message.Id] {
			local.rcvdPkts++
			continue
		}

		// Higher serial than expected. Increment til we catch up.
		for message.Serial > serialNumbers[message.Id] {
			matches := findPacket(serialNumbers, workWindow, feedWindow, position, message.Id)
			if matches == 0 { // packet loss
				local.dropPkts++
				serialNumbers[message.Id]++
				continue
			}
			if matches == 1 { // re-order
				local.reordPkts++
				local.rcvdPkts++
				serialNumbers[message.Id]++
				continue
			}
			if matches > 1 { // re-order and duplicates
				local.reordPkts++
				local.dupPkts = local.dupPkts + matches
				local.rcvdPkts++
				serialNumbers[message.Id]++
				continue
			}
		}

		// Expected serial.
		local.rcvdPkts++
		local.dupPkts = local.dupPkts + findPacket(serialNumbers, workWindow, feedWindow, position+1, message.Id)
		serialNumbers[message.Id]++
	}
	return local
}

func fastForward(serialNumbers map[int64]int64, workWindow []payload) int64 {
	var dropPkts int64
	lowest := make(map[int64]int64)

	// Populate lowest with the lowest serial number
	// for each Id in workWindow.
	for _, v := range workWindow {
		_, ok := lowest[v.Id]
		if !ok {
			lowest[v.Id] = v.Serial
		} else {
			if v.Serial < lowest[v.Id] {
				lowest[v.Id] = v.Serial
			}
		}
	}

	// Compare expected serial numbers with the lowest number found.
	// If there are packets missing increment drop counter and update
	// the expected serial number.
	for id, _ := range lowest {
		_, ok := serialNumbers[id]
		if ok && (serialNumbers[id] < lowest[id]) {
			dropPkts = dropPkts + (lowest[id] - serialNumbers[id])
			serialNumbers[id] = lowest[id]
		}
	}
	return dropPkts
}

func findPacket(serialNumbers map[int64]int64, workWindow []payload, feedWindow []payload, position int, id int64) int64 {
	var n int64 // number of matching packets

	for _, v := range workWindow[position:] {
		if v.Id == id {
			if v.Serial == serialNumbers[v.Id] {
				n++
			}
		}
	}
	for _, v := range feedWindow {
		if v.Id == id {
			if v.Serial == serialNumbers[v.Id] {
				n++
			}
		}
	}
	return n
}

func statsPrint(stats *packetStats, printJson string, qlen int, qcap int) {
	if stats.rcvdPkts == 0 {
		return
	}

	if printJson == "text" {
		fmt.Print("received: ", stats.rcvdPkts)
		fmt.Print(" dropped: ", stats.dropPkts)
		fmt.Printf("(%.2f%%) ", float64(stats.dropPkts)/float64(stats.rcvdPkts+stats.dropPkts)*100)
		fmt.Print("re-ordered: ", stats.reordPkts)
		fmt.Printf("(%.2f%%) ", float64(stats.reordPkts)/float64(stats.rcvdPkts+stats.dropPkts)*100)
		fmt.Print("duplicates: ", stats.dupPkts)

		avgRtt := stats.totRtt / time.Duration(stats.rcvdPkts)
		fastest := stats.minRtt - avgRtt // time below avg rtt
		slowest := stats.maxRtt - avgRtt // time above avg rtt
		fmt.Print(" avg rtt: ", avgRtt, " fastest: ", fastest, " slowest: +", slowest)
		fmt.Print(" queue: ", qlen, "/", qcap)
		fmt.Print(" qdrops: ", stats.pbdropPkts)
		fmt.Println()

	} else {
		output := jsonReport{}
		output.Tag = printJson
		output.TimestampUtc = time.Now().UTC()
		output.DroppedPackets = stats.dropPkts
		output.DuplicatePackets = stats.dupPkts
		output.ReorderedPackets = stats.reordPkts
		output.ReceivedPackets = stats.rcvdPkts
		// RTT in ms
		output.AverageRTT = float64(stats.totRtt/time.Duration(stats.rcvdPkts)) / 1000000
		output.LowestRTT = float64(stats.minRtt) / 1000000
		output.HighestRTT = float64(stats.maxRtt) / 1000000

		output.PBQueueLength = qlen
		output.PBQueueCapacity = qcap
		output.PBQueueDroppedPackets = int(stats.pbdropPkts)

		b, err := json.Marshal(output)
		if err != nil {
			fmt.Println("statsPrint error:", err)
		} else {
			os.Stdout.Write(b)
			fmt.Println()
		}
	}
}

func statsUpdate(global *packetStats, local packetStats) {
	global.dropPkts = global.dropPkts + local.dropPkts
	global.dupPkts = global.dupPkts + local.dupPkts
	global.reordPkts = global.reordPkts + local.reordPkts
	global.rcvdPkts = global.rcvdPkts + local.rcvdPkts
	global.totRtt = global.totRtt + local.totRtt
	if local.minRtt < global.minRtt || global.minRtt == 0 {
		global.minRtt = local.minRtt
	}
	if local.maxRtt > global.maxRtt {
		global.maxRtt = local.maxRtt
	}

	global.pbdropPkts = global.pbdropPkts + local.pbdropPkts
}

func updateRtt(message payload, local *packetStats) {
	rtt := message.Rts.Sub(message.Cts)

	local.totRtt = local.totRtt + rtt
	if rtt < local.minRtt || local.minRtt == 0 {
		local.minRtt = rtt
	}
	if rtt > local.maxRtt {
		local.maxRtt = rtt
	}
}
