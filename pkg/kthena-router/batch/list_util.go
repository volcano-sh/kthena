/*
Copyright The Volcano Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package batch

// normalizeListOpts applies list defaults and clamps limit/order.
func normalizeListOpts(opts ListOptions) (limit int, order string) {
	limit = opts.Limit
	if limit <= 0 {
		limit = DefaultListLimit
	}
	if limit < MinListLimit {
		limit = MinListLimit
	}
	if limit > MaxListLimit {
		limit = MaxListLimit
	}
	order = opts.Order
	if order == "" {
		order = OrderDesc
	}
	return limit, order
}

// lessCreatedAtID orders by created_at, breaking ties on id.
func lessCreatedAtID(order string, aCreated, bCreated int64, aID, bID string) bool {
	if order == OrderAsc {
		if aCreated == bCreated {
			return aID < bID
		}
		return aCreated < bCreated
	}
	if aCreated == bCreated {
		return aID > bID
	}
	return aCreated > bCreated
}

// applyCursorAfter drops items up to and including the item with id == after.
func applyCursorAfter[T any](items []T, after string, idFn func(T) string) []T {
	if after == "" {
		return items
	}
	for i, item := range items {
		if idFn(item) == after {
			return items[i+1:]
		}
	}
	return items
}

// applyLimit truncates items to limit.
func applyLimit[T any](items []T, limit int) []T {
	if len(items) > limit {
		return items[:limit]
	}
	return items
}
