package scheduler

import (
	"math/big"
	"sort"
)

func pendingContent(s *State) []Content {
	result := make([]Content, 0, s.Content.Len())
	for _, item := range s.Content.Values() {
		if _, done := s.Completed[item.ID]; !done {
			result = append(result, item)
		}
	}
	return result
}

func availableContent(s *State, quote Quote) []Content {
	seedPending := false
	for _, item := range s.Content.Values() {
		if item.Kind == "seed" {
			if _, done := s.Completed[item.ID]; !done {
				seedPending = true
				break
			}
		}
	}
	result := make([]Content, 0)
	for _, item := range pendingContent(s) {
		if quote.ContentIDs != nil && !containsString(quote.ContentIDs, item.ID) {
			continue
		}
		if item.Kind == "seed" || !seedPending {
			result = append(result, item)
		}
	}
	return result
}

// sellerSpeed is the cumulative accepted block bytes divided by transfer time.
// Not measured returns nil; measured but zero bytes returns 0.
func sellerSpeed(s *State, sellerID string) *float64 {
	seller := s.Sellers[sellerID]
	if seller == nil || !seller.Measured {
		return nil
	}
	value := 0.0
	if seller.TimeMsTotal > 0 {
		value = float64(seller.BytesTotal) * 1000 / float64(seller.TimeMsTotal)
	}
	return &value
}

func hasArchive(s *State, sellerID string) bool {
	seller := s.Sellers[sellerID]
	return seller != nil && seller.Measured
}

func needsProbe(s *State, sellerID string) bool { return !hasArchive(s, sellerID) }

func effectivePrice(s *State) *big.Int {
	if s.Price.Mode == "manual" && s.Price.ManualValue != nil {
		return sats(*s.Price.ManualValue)
	}
	return sats(s.Config.MaxBlockPrice)
}

func seatCap(s *State) int64 { return s.Config.BaseSeats + s.Config.FloatingSeats }

func unmeasuredCap(s *State) int64 { return s.Config.FloatingSeats }

// validQuotes returns market quotes for online, non-blacklisted sellers whose
// quote is still usable and covers pending content.
func validQuotes(s *State) []Quote {
	result := make([]Quote, 0)
	for _, seller := range s.Sellers {
		if !seller.Online || seller.Blacklisted {
			continue
		}
		var pool *Pool
		for _, candidate := range sortedPools(s) {
			if candidate.SellerID == seller.ID && (candidate.Funding == "open" || candidate.Funding == "opening") &&
				candidate.Scheduling != "draining" && candidate.Scheduling != "retired" {
				pool = candidate
				break
			}
		}
		var quote *Quote
		if pool != nil {
			quote = &pool.Quote
		} else {
			quote = seller.Quote
		}
		if quote == nil {
			continue
		}
		if pool == nil && quote.ExpiresAt <= s.Now {
			continue
		}
		if len(availableContent(s, *quote)) == 0 {
			continue
		}
		result = append(result, *quote)
	}
	return result
}

func byPrice(a, b Quote) int {
	cmp := sats(a.BlockPrice).Cmp(sats(b.BlockPrice))
	if cmp < 0 {
		return -1
	}
	if cmp > 0 {
		return 1
	}
	return compareStrings(a.SellerID, b.SellerID)
}

func totalSpeed(s *State, quotes []Quote) float64 {
	seen := make(map[string]bool, len(quotes))
	total := 0.0
	for _, quote := range quotes {
		if seen[quote.SellerID] {
			continue
		}
		seen[quote.SellerID] = true
		if speed := sellerSpeed(s, quote.SellerID); speed != nil {
			total += *speed
		}
	}
	return total
}

func recommendations(s *State) (SliderView, []Quote, *big.Int) {
	effective := effectivePrice(s)
	market := validQuotes(s)
	sort.SliceStable(market, func(i, j int) bool { return byPrice(market[i], market[j]) < 0 })
	quotes := make([]Quote, 0, len(market))
	for _, quote := range market {
		if sats(quote.BlockPrice).Cmp(effective) <= 0 {
			quotes = append(quotes, quote)
		}
	}
	price := SliderView{
		Mode:        s.Price.Mode,
		ManualValue: s.Price.ManualValue,
		Effective:   effective.String(),
	}
	if len(market) > 0 {
		price.Range = &PriceRange{Min: market[0].BlockPrice, Max: market[len(market)-1].BlockPrice}
	}
	return price, quotes, effective
}

func view(s *State) View {
	priceView, quotes, effective := recommendations(s)
	pools := sortedPools(s)
	live := make([]*Pool, 0, len(pools))
	problem := make([]*Pool, 0)
	for _, pool := range pools {
		if pool.Funding != "closed" && pool.Funding != "failed" && pool.Funding != "problem" {
			live = append(live, pool)
		}
		if pool.Funding == "problem" {
			problem = append(problem, pool)
		}
	}
	complete := s.ManifestKnown && len(pendingContent(s)) == 0
	requests := make([]*Request, 0)
	for _, request := range sortedRequests(s) {
		if request.State == "pending" || request.State == "unresolved" {
			requests = append(requests, request)
		}
	}
	serving := make([]*Pool, 0)
	active := make([]*Pool, 0)
	for _, pool := range live {
		if pool.Funding == "open" && pool.RequestID != nil && pool.Scheduling == "serving" {
			serving = append(serving, pool)
		}
		if pool.Funding == "open" && pool.Scheduling != "draining" && pool.Scheduling != "retired" {
			active = append(active, pool)
		}
	}
	reasons := make([]string, 0)
	if !s.ManifestKnown {
		reasons = append(reasons, "manifest-unknown")
	}
	if len(problem) > 0 {
		reasons = append(reasons, "problem-pool")
	}
	if len(quotes) == 0 && !complete {
		reasons = append(reasons, "no-eligible-seller")
	}
	hasUnmeasured := false
	for _, quote := range quotes {
		if sellerSpeed(s, quote.SellerID) == nil {
			hasUnmeasured = true
			break
		}
	}
	if hasUnmeasured {
		reasons = append(reasons, "speed-unmeasured")
	}
	if s.Price.Mode == "manual" && s.Price.ManualValue != nil {
		limit := sats(*s.Price.ManualValue)
		for _, seller := range s.Sellers {
			if seller.Online && !seller.Blacklisted && seller.Quote != nil && sats(seller.Quote.BlockPrice).Cmp(limit) > 0 {
				reasons = append(reasons, "price-limit")
				break
			}
		}
	}
	if !complete && len(requests) == 0 && s.Intent == "run" {
		opening := false
		settling := false
		for _, pool := range live {
			if pool.Funding == "opening" {
				opening = true
			}
			if pool.Funding == "closing" || pool.Scheduling == "draining" {
				settling = true
			}
		}
		if opening {
			reasons = append(reasons, "opening")
		} else if settling {
			reasons = append(reasons, "settling")
		} else {
			reasons = append(reasons, "awaiting-resources")
		}
	}
	costMin := big.NewInt(0)
	costMax := big.NewInt(0)
	covered := true
	for _, item := range pendingContent(s) {
		var request *Request
		for _, candidate := range sortedRequests(s) {
			if (candidate.State == "pending" || candidate.State == "unresolved") && containsString(candidate.ContentIDs, item.ID) {
				request = candidate
				break
			}
		}
		prices := make([]*big.Int, 0)
		if request != nil {
			prices = append(prices, sats(request.Prices[item.ID]))
		} else {
			for _, quote := range quotes {
				if quote.ContentIDs != nil && !containsString(quote.ContentIDs, item.ID) {
					continue
				}
				prices = append(prices, price(quote, item))
			}
		}
		if len(prices) == 0 {
			covered = false
			break
		}
		minimum := prices[0]
		maximum := prices[0]
		for _, value := range prices[1:] {
			if value.Cmp(minimum) < 0 {
				minimum = value
			}
			if value.Cmp(maximum) > 0 {
				maximum = value
			}
		}
		costMin.Add(costMin, minimum)
		costMax.Add(costMax, maximum)
	}
	aboveCeiling := 0
	for _, seller := range s.Sellers {
		if seller.Online && !seller.Blacklisted && seller.Quote != nil && sats(seller.Quote.BlockPrice).Cmp(effective) > 0 {
			aboveCeiling++
		}
	}
	eligible := make(map[string]bool)
	for _, quote := range quotes {
		eligible[quote.SellerID] = true
	}
	measured := 0
	zeroSpeed := 0
	unmeasuredEligible := 0
	speeds := make(map[string]SellerSpeedView, len(s.Sellers))
	blacklisted := 0
	for id, seller := range s.Sellers {
		speed := sellerSpeed(s, id)
		if speed != nil {
			measured++
			if *speed == 0 {
				zeroSpeed++
			}
		}
		if seller.Blacklisted {
			blacklisted++
		}
		speeds[id] = SellerSpeedView{
			BytesPerSecond: speed,
			Measured:       seller.Measured,
			BytesTotal:     seller.BytesTotal,
			TimeMsTotal:    seller.TimeMsTotal,
			LastAt:         seller.LastAt,
		}
	}
	for _, quote := range quotes {
		if sellerSpeed(s, quote.SellerID) == nil {
			unmeasuredEligible++
		}
	}
	execution := "waiting"
	switch {
	case complete:
		execution = "finished"
	case s.Intent == "ready":
		execution = "ready"
	case s.Intent == "cancel":
		if len(requests) > 0 {
			execution = "stopping"
		} else {
			execution = "stopped"
		}
	case s.Intent == "pause":
		if len(requests) > 0 {
			execution = "stopping"
		} else {
			execution = "paused"
		}
	default:
		if len(requests) > 0 {
			execution = "running"
		}
	}
	contentState := "incomplete"
	if !s.ManifestKnown {
		contentState = "manifest-unknown"
	} else if complete {
		contentState = "complete"
	}
	funds := "none"
	hasClosed := false
	for _, pool := range pools {
		if pool.Funding == "closed" && !pool.RefundAvailable {
			hasClosed = true
		}
	}
	if len(problem) > 0 {
		funds = "problem"
	} else if len(live) > 0 || hasClosed {
		funds = "pending"
	} else if len(pools) > 0 {
		funds = "settled"
	}
	locked := big.NewInt(0)
	committed := big.NewInt(0)
	pendingReturn := big.NewInt(0)
	arbitrationReserved := big.NewInt(0)
	settlementReserved := big.NewInt(0)
	for _, pool := range live {
		locked.Add(locked, sats(pool.Amount))
		committed.Add(committed, sats(pool.Committed))
		arbitrationReserved.Add(arbitrationReserved, sats(pool.Fees.ArbitrationBase))
		count := int64(1)
		if pool.RequestID != nil {
			if request := s.Requests[*pool.RequestID]; request != nil {
				count = int64(len(request.ContentIDs))
			}
		}
		arbitrationReserved.Add(arbitrationReserved,
			new(big.Int).Mul(sats(pool.Fees.ArbitrationPerItem), big.NewInt(count)))
		settlementReserved.Add(settlementReserved, sats(pool.Fees.Settlement))
	}
	for _, pool := range pools {
		if pool.Funding == "closed" && !pool.RefundAvailable && pool.Returned != nil {
			pendingReturn.Add(pendingReturn, sats(*pool.Returned))
		}
	}
	var estimate *EstimatedContent
	if covered && s.ManifestKnown {
		estimate = &EstimatedContent{Min: costMin.String(), Max: costMax.String()}
	}
	return View{
		TaskID:                    s.TaskID,
		Revision:                  s.Revision,
		Execution:                 execution,
		Content:                   contentState,
		Funds:                     funds,
		Price:                     priceView,
		SellerSpeeds:              speeds,
		ActualBytesPerSecond:      totalSpeed(s, poolQuotes(serving)),
		SustainableBytesPerSecond: totalSpeed(s, poolQuotes(active)),
		Facts: ViewFacts{
			Eligible:           len(eligible),
			AboveCeiling:       aboveCeiling,
			Measured:           measured,
			UnmeasuredEligible: unmeasuredEligible,
			ZeroSpeed:          zeroSpeed,
			Blacklisted:        blacklisted,
			InFlight:           len(requests),
		},
		Completed: len(s.Completed),
		Total:     s.Content.Len(),
		Reasons:   reasons,
		Money: ViewMoney{
			Spent:               s.Spent,
			Locked:              locked.String(),
			Committed:           committed.String(),
			PendingReturn:       pendingReturn.String(),
			ArbitrationReserved: arbitrationReserved.String(),
			SettlementReserved:  settlementReserved.String(),
		},
		EstimatedRemainingContent: estimate,
	}
}

func poolQuotes(pools []*Pool) []Quote {
	quotes := make([]Quote, 0, len(pools))
	for _, pool := range pools {
		quotes = append(quotes, pool.Quote)
	}
	return quotes
}
