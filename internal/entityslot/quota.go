package entityslot

import "sort"

// distributeQuota splits the slot quota among the queried entities PROPORTIONALLY
// TO WEIGHT; NO queried entity gets 0 (floor 1) and the total is
// EXACT.
//
// Algorithm:
//  1. Pruning: if len(order) > total pick the heaviest total ones (drop the lighter).
//  2. Floor: 1 for each queried one (cost = len(queried) <= total).
//  3. Distribute the rest: floor-proportional distribution + largest remainder method (largest
//     remainder first, heavier weight first on ties).
//  4. Result: sum = total, none is 0, weights are reflected.
//
// Why floor 1: weak but real interests should not go completely silent.
// Why proportional to weight: if the profile says "this person is 70% Fenerbahce" the feed should
// reflect it — an equal split would throw away the profile's weight-producing effort.
// Why largest remainder: once pruning + floor are satisfied the remainder is small,
// and the largest-remainder approach minimizes the error.
//
// Precedent: the favorite-author quota distribution applies the same floor idea but
// **the floor sum can exceed the budget** (no cap trimming). Here there is NO trimming
// because the floor guarantees it: thanks to pruning len(queried)<=total
// ALWAYS. Since it originates from the Fetcher this number is directly the candidate
// budget and does not spread excessively.
func distributeQuota(weights map[string]float64, order []string, total int) map[string]int {
	if len(order) == 0 || total <= 0 {
		return nil
	}

	// Step 1: Pruning — if len(order) > total pick the heaviest total ones
	queried := order
	if len(queried) > total {
		queried = append([]string(nil), order...)
		sort.SliceStable(queried, func(i, j int) bool {
			return weights[queried[i]] > weights[queried[j]]
		})
		queried = queried[:total]
	}

	var totalWeight float64
	for _, e := range queried {
		totalWeight += weights[e]
	}

	// Step 2: Floor — 1 for each entity
	quota := make(map[string]int, len(queried))
	for _, e := range queried {
		quota[e] = 1
	}
	allocated := len(queried) // floor cost
	remaining := total - allocated

	// If the floor equals the total, we are done
	if remaining == 0 {
		return quota
	}

	// Step 3: Distribute the rest (proportional + largest remainder)

	if totalWeight <= 0 {
		// No weight (profile exists but Stable is empty): split the rest equally
		perEntity := remaining / len(queried)
		leftover := remaining % len(queried)

		for _, e := range queried {
			quota[e] += perEntity
		}
		// Distribute leftovers in order (determinism)
		for i := 0; i < leftover; i++ {
			quota[queried[i]]++
		}
	} else {
		// Weight exists: proportional distribution (floor + largest remainder)
		contributions := make(map[string]int, len(queried))
		floorTotal := 0

		// Floor: the integer part of each entity's proportional share
		for _, e := range queried {
			frac := float64(remaining) * (weights[e] / totalWeight)
			floorVal := int(frac)
			contributions[e] = floorVal
			floorTotal += floorVal
		}

		// Distribute remainders (largest remainder method)
		leftover := remaining - floorTotal
		if leftover > 0 {
			type rem struct {
				entity string
				value  float64
			}
			rems := make([]rem, 0, len(queried))

			for _, e := range queried {
				frac := float64(remaining) * (weights[e] / totalWeight)
				remainder := frac - float64(int(frac))
				rems = append(rems, rem{e, remainder})
			}

			// Sorting: remainder descending, weight descending on ties
			sort.SliceStable(rems, func(i, j int) bool {
				if rems[i].value != rems[j].value {
					return rems[i].value > rems[j].value
				}
				return weights[rems[i].entity] > weights[rems[j].entity]
			})

			// +1 to remainders up to the leftover count
			for i := 0; i < leftover; i++ {
				contributions[rems[i].entity]++
			}
		}

		// Add to the quota

		for _, e := range queried {
			quota[e] += contributions[e]
		}
	}

	return quota
}
