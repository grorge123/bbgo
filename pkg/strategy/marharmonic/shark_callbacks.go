// Code generated by "callbackgen -type SHARK"; DO NOT EDIT.

package marharmonic

import ()

func (inc *SHARK) OnUpdate(cb func(value float64)) {
	inc.updateCallbacks = append(inc.updateCallbacks, cb)
}

func (inc *SHARK) EmitUpdate(value float64) {
	for _, cb := range inc.updateCallbacks {
		cb(value)
	}
}
