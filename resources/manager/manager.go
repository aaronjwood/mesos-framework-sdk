package manager

import (
	"errors"
	"mesos-framework-sdk/include/mesos_v1"
	"mesos-framework-sdk/structures"
	"mesos-framework-sdk/task"
	"strconv"
	"strings"
)

/*
The resource manager will handle offers and allocate it to a task.
*/

type ResourceManager interface {
	AddOffers(offers []*mesos_v1.Offer)
	HasResources() bool
	AddFilter(t *mesos_v1.TaskInfo, filters []task.Filter) error
	ClearFilters(t *mesos_v1.TaskInfo)
	Assign(task *mesos_v1.TaskInfo) (*mesos_v1.Offer, error)
	Offers() []*mesos_v1.Offer
}

// This cleans up the logic for the offer->resource matching.
type MesosOfferResources struct {
	Offer    *mesos_v1.Offer
	Cpu      float64
	Mem      float64
	Disk     *mesos_v1.Resource_DiskInfo
	Accepted bool
}

type DefaultResourceManager struct {
	offers   []*MesosOfferResources
	filterOn structures.DistributedMap
	strategy structures.DistributedMap
}

// NOTE (tim): Filter types follow VALUE_TYPE's defined in mesos
const (
	SCALAR = mesos_v1.Value_SCALAR
	TEXT   = mesos_v1.Value_TEXT
	RANGES = mesos_v1.Value_RANGES
	SET    = mesos_v1.Value_SET
)

func NewDefaultResourceManager() *DefaultResourceManager {
	return &DefaultResourceManager{
		offers:   make([]*MesosOfferResources, 0),
		filterOn: structures.NewConcurrentMap(0),
		strategy: structures.NewConcurrentMap(0),
	}
}

// Add in a new batch of offers
func (d *DefaultResourceManager) AddOffers(offers []*mesos_v1.Offer) {
	// No matter what, we clear offers on this call to make sure
	// we don't have stale offers that are already declined.
	d.clearOffers()
	for _, offer := range offers {
		mesosOffer := &MesosOfferResources{}
		for _, resource := range offer.Resources {
			switch resource.GetName() {
			case "cpus":
				mesosOffer.Cpu = resource.GetScalar().GetValue()
			case "mem":
				mesosOffer.Mem = resource.GetScalar().GetValue()
			case "disk":
				mesosOffer.Disk = resource.GetDisk()
			}
		}
		mesosOffer.Offer = offer
		d.offers = append(d.offers, mesosOffer)
	}
}

// Clear out existing offers if any exist.
func (d *DefaultResourceManager) clearOffers() {
	d.offers = nil // Release memory to the GC.
}

// Do we have any resources left?
func (d *DefaultResourceManager) HasResources() bool {
	return len(d.offers) > 0
}

// Tells our resource manager to apply filters to this task.
func (d *DefaultResourceManager) AddFilter(t *mesos_v1.TaskInfo, filters []task.Filter) error {
	for _, f := range filters { // Check all filters
		switch strings.ToLower(f.Type) {
		case "ranges", "set", "text", "scalar":
			val := d.filterOn.Get(t.GetName())
			// Initial set, append set
			if val == nil {
				d.filterOn.Set(t.GetName(), []task.Filter{{Type: f.Type, Value: f.Value}})
			} else {
				list := val.([]task.Filter)
				list = append(list, task.Filter{Type: f.Type, Value: f.Value})
				d.filterOn.Set(t.GetName(), list)
			}
		case "strategy":
			d.strategy.Set(t.GetName(), f.Value[0])
		default:
			return errors.New("Invalid filter passed in: " + f.Type + ". Allowed filters are SCALAR, TEXT, SET, RANGES, and STRATEGY.")
		}
	}

	return nil
}

func (d *DefaultResourceManager) ClearFilters(t *mesos_v1.TaskInfo) {
	d.filterOn.Delete(t.GetName()) // Deletes all filters on a task.
	d.strategy.Delete(t.GetName()) // Deletes the strategy.
}

// Swaps current element with last, then sets the entire slice to the slice without the last element.
// Faster than taking two slices around the element and re-combining them since no resizing occurs
// and we don't care about order.
func (d *DefaultResourceManager) popOffer(i int) {
	d.offers[len(d.offers)-1], d.offers[i] = d.offers[i], d.offers[len(d.offers)-1]
	d.offers = d.offers[:len(d.offers)-1]
}

// Check if filter applies to a single Text attribute.
func (d *DefaultResourceManager) filterOnAttrText(f []string, a *mesos_v1.Attribute) bool {
	for _, term := range f {
		// Case insensitive
		if strings.ToLower(term) == strings.ToLower(a.GetText().GetValue()) {
			// The term we're looking for exists.
			return true
		} else {
			// Immediately return false if not all match.
			return false
		}
	}
	return false
}

// Check if filter applies to a single Scalar attribute.
func (d *DefaultResourceManager) filterOnAttrScalar(f []string, a *mesos_v1.Attribute) bool {
	for _, term := range f {
		termFloat64, err := strconv.ParseFloat(term, 64)
		if err != nil {
			// We can't parse a proper int, ignore.
			continue
		}
		if a.GetScalar().GetValue() == termFloat64 {
			return true
		}
	}
	return false
}

func (d *DefaultResourceManager) filter(f []task.Filter, offer *mesos_v1.Offer) bool {
	for _, filter := range f {
		// Range over all of our attributes.
		for _, attr := range offer.Attributes {
			switch attr.GetType() {
			case SCALAR:
			case TEXT:
				if d.filterOnAttrText(filter.Value, attr) {
					return true
				}
			case SET:
			case RANGES:
			}
		}
	}

	return false
}

func (d *DefaultResourceManager) allocateMemResource(mem float64, offer *MesosOfferResources) bool {
	if offer.Mem-mem >= 0 {
		offer.Mem = offer.Mem - mem
		return true
	}

	return false
}

func (d *DefaultResourceManager) allocateCpuResource(cpu float64, offer *MesosOfferResources) bool {
	if offer.Cpu-cpu >= 0 {
		offer.Cpu = offer.Cpu - cpu
		return true
	}

	return false
}

func (d *DefaultResourceManager) allocateDiskResource(resource *mesos_v1.Resource, offer *MesosOfferResources) bool {
	if resource.Disk != nil {
		offer.Disk = resource.Disk
		return true
	}

	return false
}

// Assign an offer to a task.
func (d *DefaultResourceManager) Assign(mesosTask *mesos_v1.TaskInfo) (*mesos_v1.Offer, error) {
L:
	for i, offer := range d.offers {

		// If this task has filters, make sure to filter on them.
		if filter := d.filterOn.Get(mesosTask.GetName()); filter != nil {
			validOffer := d.filter(filter.([]task.Filter), offer.Offer)
			if !validOffer {

				// We don't care about this offer since it does't match our params.
				continue L
			}
		}

		// Eat up this offer's resources with the task's needs.
		for _, resource := range mesosTask.Resources {
			res := resource.GetScalar().GetValue()

			switch resource.GetName() {
			case "cpus":
				if d.allocateCpuResource(res, offer) {
					break
				}

				// We can't use this offer if it has no CPUs, move on to the next offer.
				continue L
			case "mem":
				if d.allocateMemResource(res, offer) {
					break
				}

				// We can't use this offer if it has no memory, move on to the next offer.
				continue L
			case "disk":
				d.allocateDiskResource(resource, offer)
			}
		}

		// Mark this offer as accepted so that it's not returned as part of the remaining offers.
		d.offers[i].Accepted = true

		// Remove the offer if it has no resources for other tasks to eat.
		exists := d.strategy.Get(mesosTask.GetName())
		var strategy string
		if exists == nil {
			strategy = "non-mux"
		} else {
			strategy = exists.(string)
		}
		if !strings.EqualFold(strategy, "mux") {
			d.popOffer(i)
		} else if offer.Mem == 0 || offer.Cpu == 0 {
			d.popOffer(i)
		}

		return offer.Offer, nil
	}

	return nil, errors.New("Cannot find a suitable offer for task " + mesosTask.GetName())
}

// Returns a list of offers that have not been altered and returned to the client for accept calls.
func (d *DefaultResourceManager) Offers() (offers []*mesos_v1.Offer) {
	for _, o := range d.offers {
		if !o.Accepted {
			offers = append(offers, o.Offer)
		}
	}
	return offers
}
