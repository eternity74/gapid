package vulkan

import (
	"context"
	"fmt"
	"reflect"

	"github.com/google/gapid/core/app/benchmark"
	"github.com/google/gapid/core/log"
	"github.com/google/gapid/gapis/atom"
	"github.com/google/gapid/gapis/capture"
	"github.com/google/gapid/gapis/config"
	"github.com/google/gapid/gapis/database"
	"github.com/google/gapid/gapis/gfxapi"
)

var dependencyGraphBuildCounter = benchmark.GlobalCounters.Duration("dependencyGraph.build")

type StateAddress uint32

// To conform with the DCE interface of GLES, here we define Vulkan handles
// as stateKeys. For device memories and command buffers, type composition is
// needed.
type stateKey interface {
	Parent() stateKey
}

type vulkanStateKey uint64

func (h vulkanStateKey) Parent() stateKey {
	return nil
}

// Device memory composition hierarchy (parent -> child)
// vulkanDeviceMemory -> vulkanDeviceMemoryHandle
//                   \-> vulkanDeviceMemoryBinding -> vulkanDeviceMemoryData
type vulkanDeviceMemory struct {
	handle   *vulkanDeviceMemoryHandle
	bindings map[uint64][]*vulkanDeviceMemoryBinding // map from offsets to a list of memory bindings
}

type vulkanDeviceMemoryHandle struct {
	memory         *vulkanDeviceMemory
	vkDeviceMemory VkDeviceMemory
}

type vulkanDeviceMemoryBinding struct {
	memory *vulkanDeviceMemory
	start  uint64
	end    uint64
	data   *vulkanDeviceMemoryData
}

var emptyMemoryBindings = []*vulkanDeviceMemoryBinding{}

type vulkanDeviceMemoryData struct {
	binding *vulkanDeviceMemoryBinding
}

func (m *vulkanDeviceMemory) Parent() stateKey {
	return nil
}

func (h *vulkanDeviceMemoryHandle) Parent() stateKey {
	return h.memory
}

func (b *vulkanDeviceMemoryBinding) Parent() stateKey {
	return b.memory
}

func (d *vulkanDeviceMemoryData) Parent() stateKey {
	return d.binding
}

func newVulkanDeviceMemory(handle VkDeviceMemory) *vulkanDeviceMemory {
	m := &vulkanDeviceMemory{handle: nil, bindings: map[uint64][]*vulkanDeviceMemoryBinding{}}
	m.handle = &vulkanDeviceMemoryHandle{memory: m, vkDeviceMemory: handle}
	return m
}

func (m *vulkanDeviceMemory) addBinding(offset, size uint64) *vulkanDeviceMemoryBinding {
	newBinding := &vulkanDeviceMemoryBinding{
		memory: m,
		start:  offset,
		end:    offset + size,
		data:   nil}
	newBinding.data = &vulkanDeviceMemoryData{binding: newBinding}
	m.bindings[offset] = append(m.bindings[offset], newBinding)
	return newBinding
}

func (m *vulkanDeviceMemory) getOverlappedBindings(offset, size uint64) []*vulkanDeviceMemoryBinding {
	overlappedBindings := []*vulkanDeviceMemoryBinding{}
	for _, bl := range m.bindings {
		for _, b := range bl {
			if overlap(b.start, b.end, offset, offset+size) {
				overlappedBindings = append(overlappedBindings, b)
			}
		}
	}
	return overlappedBindings
}

func overlap(startA, endA, startB, endB uint64) bool {
	if (startA < endB && startA >= startB) ||
		(endA < endB && endA >= startB) ||
		(startB < startA && startB >= startA) ||
		(endB < endA && endB >= startA) {
		return true
	}
	return false
}

// Command buffer composition hierachy (parent -> child):
// vulkanCommandBuffer -> vulkanCommandBufferHandle
//                    \-> vulkanRecordedCommands
type vulkanCommandBuffer struct {
	handle  *vulkanCommandBufferHandle
	records *vulkanRecordedCommands
}

type vulkanCommandBufferHandle struct {
	CommandBuffer   *vulkanCommandBuffer
	vkCommandBuffer VkCommandBuffer
}

type vulkanRecordedCommands struct {
	CommandBuffer *vulkanCommandBuffer
	Commands      []func(b *AtomBehaviour)
}

func newVulkanCommandBuffer(handle VkCommandBuffer) *vulkanCommandBuffer {
	cb := &vulkanCommandBuffer{handle: nil, records: nil}
	cb.handle = &vulkanCommandBufferHandle{CommandBuffer: cb, vkCommandBuffer: handle}
	cb.records = &vulkanRecordedCommands{CommandBuffer: cb, Commands: []func(b *AtomBehaviour){}}
	return cb
}

func (cb *vulkanCommandBuffer) Parent() stateKey {
	return nil
}

func (h *vulkanCommandBufferHandle) Parent() stateKey {
	return h.CommandBuffer
}

func (c *vulkanRecordedCommands) Parent() stateKey {
	return c.CommandBuffer
}

func (c *vulkanRecordedCommands) appendCommand(f func(b *AtomBehaviour)) *vulkanRecordedCommands {
	c.Commands = append(c.Commands, f)
	return c
}

// Dependency graph and the node type in the graph
// TODO(qining): Move the dependency graph and other types, which are shared
// with GLES, to another proper place.
const nullStateAddress = StateAddress(0)

type DependencyGraph struct {
	atoms          []atom.Atom           // Atom list which this graph was build for.
	behaviours     []AtomBehaviour       // State reads/writes for each atom (graph edges).
	roots          map[StateAddress]bool // State to mark live at requested atoms.
	addressMap     addressMapping        // Remap state keys to integers for performance.
	deviceMemories map[VkDeviceMemory]*vulkanDeviceMemory
	commandBuffers map[VkCommandBuffer]*vulkanCommandBuffer
}

type AtomBehaviour struct {
	Read      []StateAddress // State read by an atom.
	Modify    []StateAddress // State read and written by an atom.
	Write     []StateAddress // State written by an atom.
	KeepAlive bool           // Force the atom to be live.
	Aborted   bool           // Mutation of this command aborts.
}

type addressMapping struct {
	address map[stateKey]StateAddress
	key     map[StateAddress]stateKey
	parent  map[StateAddress]StateAddress
}

func (g *DependencyGraph) Print(ctx context.Context, b *AtomBehaviour) {
	for _, read := range b.Read {
		key := g.addressMap.key[read]
		log.I(ctx, " - read [%v]%T%+v", read, key, key)
	}
	for _, modify := range b.Modify {
		key := g.addressMap.key[modify]
		log.I(ctx, " - modify [%v]%T%+v", modify, key, key)
	}
	for _, write := range b.Write {
		key := g.addressMap.key[write]
		log.I(ctx, " - write [%v]%T%+v", write, key, key)
	}
	if b.Aborted {
		log.I(ctx, " - aborted")
	}
}

// For a given Vulkan handle of device memory, returns the corresponding
// stateKey of the device memory if it has been created and added to the graph
// before. Otherwise, creates and adds the stateKey for the handle and returns
// the new created stateKey
func (g *DependencyGraph) getOrCreateDeviceMemory(handle VkDeviceMemory) *vulkanDeviceMemory {
	if m, ok := g.deviceMemories[handle]; ok {
		return m
	}
	newM := newVulkanDeviceMemory(handle)
	g.deviceMemories[handle] = newM
	return newM
}

// For a given Vulkan handle of command buffer, returns the corresponding
// stateKey of the command buffer if it has been created and added to the graph
// before. Otherwise, creates and adds the stateKey for the handle and returns
// the new created stateKey
func (g *DependencyGraph) getOrCreateCommandBuffer(handle VkCommandBuffer) *vulkanCommandBuffer {
	if cb, ok := g.commandBuffers[handle]; ok {
		return cb
	}
	newCb := newVulkanCommandBuffer(handle)
	g.commandBuffers[handle] = newCb
	return newCb
}

// The public accessible entrance of building a dep graph from atom list
func GetDependencyGraph(ctx context.Context) (*DependencyGraph, error) {
	r, err := database.Build(ctx, &DependencyGraphResolvable{Capture: capture.Get(ctx)})
	if err != nil {
		return nil, fmt.Errorf("Could not calculate dependency graph: %v", err)
	}
	return r.(*DependencyGraph), nil
}

// The real entrance of dep graph building
func (r *DependencyGraphResolvable) Resolve(ctx context.Context) (interface{}, error) {
	c, err := capture.ResolveFromPath(ctx, r.Capture)
	if err != nil {
		return nil, err
	}
	atoms, err := c.Atoms(ctx)
	if err != nil {
		return nil, err
	}

	g := &DependencyGraph{
		atoms:      atoms.Atoms,
		behaviours: make([]AtomBehaviour, len(atoms.Atoms)),
		roots:      map[StateAddress]bool{},
		addressMap: addressMapping{
			address: map[stateKey]StateAddress{nil: nullStateAddress},
			key:     map[StateAddress]stateKey{nullStateAddress: nil},
			parent:  map[StateAddress]StateAddress{nullStateAddress: nullStateAddress},
		},
		deviceMemories: map[VkDeviceMemory]*vulkanDeviceMemory{},
		commandBuffers: map[VkCommandBuffer]*vulkanCommandBuffer{},
	}

	s := c.NewState()
	t0 := dependencyGraphBuildCounter.Start()
	for i, a := range g.atoms {
		g.behaviours[i] = g.getBehaviour(ctx, s, atom.ID(i), a)
	}
	dependencyGraphBuildCounter.Stop(t0)
	return g, nil
}

// State address is assigned in the function addressOf() and used as the
// identity of Vulkan handles (vulkan object), Device memory stateKey or
// CommandBuffer stateKey in the dependency graph.
func (m *addressMapping) addressOf(state stateKey) StateAddress {
	if a, ok := m.address[state]; ok {
		return a
	}
	address := StateAddress(len(m.address))
	m.address[state] = address
	m.key[address] = state
	m.parent[address] = m.addressOf(state.Parent())
	return address
}

func (b *AtomBehaviour) read(g *DependencyGraph, state stateKey) {
	if state != nil {
		b.Read = append(b.Read, g.addressMap.addressOf(state))
	}
}

func (b *AtomBehaviour) modify(g *DependencyGraph, state stateKey) {
	if state != nil {
		b.Modify = append(b.Modify, g.addressMap.addressOf(state))
	}
}

func (b *AtomBehaviour) write(g *DependencyGraph, state stateKey) {
	if state != nil {
		b.Write = append(b.Write, g.addressMap.addressOf(state))
	}
}

// Build the corresponding dep graph node for a given atom
// Note this function is called on a new graphics state
func (g *DependencyGraph) getBehaviour(ctx context.Context, s *gfxapi.State, id atom.ID, a atom.Atom) AtomBehaviour {
	b := AtomBehaviour{}

	// Helper function for debug info logging when debug info dumpping is turned on
	debug := func(fmt string, args ...interface{}) {
		if config.DebugDeadCodeElimination {
			log.D(ctx, fmt, args...)
		}
	}

	// Wraps AtomBehaviour's read/write/modify to add debug info.
	addRead := func(b *AtomBehaviour, g *DependencyGraph, state stateKey) {
		b.read(g, state)
		debug("\tread: stateKey: %v, stateAddress: %v", state, g.addressMap.addressOf(state))
	}
	addWrite := func(b *AtomBehaviour, g *DependencyGraph, state stateKey) {
		b.write(g, state)
		debug("\twrite: stateKey: %v, stateAddress: %v", state, g.addressMap.addressOf(state))
	}
	addModify := func(b *AtomBehaviour, g *DependencyGraph, state stateKey) {
		b.modify(g, state)
		debug("\tmodify: stateKey: %v, stateAddress: %v", state, g.addressMap.addressOf(state))
	}

	// Helper function that gets overlapped memory bindings with a given offset and size
	getOverlappingMemoryBindings := func(memory VkDeviceMemory,
		offset, size uint64) []*vulkanDeviceMemoryBinding {
		return g.getOrCreateDeviceMemory(memory).getOverlappedBindings(offset, size)
	}

	// Helper function that gets the overlapped memory bindings for a given image
	getOverlappedBindingsForImage := func(image VkImage) []*vulkanDeviceMemoryBinding {
		if !GetState(s).Images.Contains(image) {
			log.E(ctx, "Error Image: %v: does not exist in state", image)
			return []*vulkanDeviceMemoryBinding{}
		}
		imageObj := GetState(s).Images.Get(image)
		if imageObj.IsSwapchainImage {
			return []*vulkanDeviceMemoryBinding{}
		} else if imageObj.BoundMemory != nil {
			boundMemory := imageObj.BoundMemory.VulkanHandle
			offset := uint64(imageObj.BoundMemoryOffset)
			size := uint64(uint64(imageObj.Size))
			return getOverlappingMemoryBindings(boundMemory, offset, size)
		} else {
			log.E(ctx, "Error Image: %v: Cannot get the bound memory for an image which has not been bound yet", image)
			return []*vulkanDeviceMemoryBinding{}
		}
	}

	// Helper function that gets the overlapped memory bindings for a given buffer
	getOverlappedBindingsForBuffer := func(buffer VkBuffer) []*vulkanDeviceMemoryBinding {
		if !GetState(s).Buffers.Contains(buffer) {
			log.E(ctx, "Error Buffer: %v: does not exist in state", buffer)
			return []*vulkanDeviceMemoryBinding{}
		}
		bufferObj := GetState(s).Buffers.Get(buffer)
		if bufferObj.Memory != nil {
			boundMemory := bufferObj.Memory.VulkanHandle
			offset := uint64(bufferObj.MemoryOffset)
			size := uint64(uint64(bufferObj.Info.Size))
			return getOverlappingMemoryBindings(boundMemory, offset, size)
		} else {
			log.E(ctx, "Error Buffer: %v: Cannot get the bound memory for a buffer which has not been bound yet", buffer)
			return []*vulkanDeviceMemoryBinding{}
		}
	}

	// Helper function that reads the given image handle, and returns the memory
	// bindings of the image
	readImageHandleAndGetBindings := func(b *AtomBehaviour, image VkImage) []*vulkanDeviceMemoryBinding {
		b.read(g, vulkanStateKey(image))
		return getOverlappedBindingsForImage(image)
	}

	// Helper function that reads the given buffer handle, and returns the memory
	// bindings of the buffer
	readBufferHandleAndGetBindings := func(b *AtomBehaviour, buffer VkBuffer) []*vulkanDeviceMemoryBinding {
		b.read(g, vulkanStateKey(buffer))
		return getOverlappedBindingsForBuffer(buffer)
	}

	// Helper function that 'read' the given memory bindings
	readMemoryBindingsData := func(pb *AtomBehaviour, bindings []*vulkanDeviceMemoryBinding) {
		for _, binding := range bindings {
			pb.read(g, binding.data)
			debug("\tread binding data: %v <-  binding: %v <- memory: %v", g.addressMap.addressOf(binding.data), g.addressMap.addressOf(binding), g.addressMap.addressOf(binding.Parent()))
		}
	}

	// Helper function that 'write' the given memory bindings
	writeMemoryBindingsData := func(pb *AtomBehaviour, bindings []*vulkanDeviceMemoryBinding) {
		for _, binding := range bindings {
			pb.write(g, binding.data)
			debug("\twrite binding data: %v <- binding: %v <- memory: %v", g.addressMap.addressOf(binding.data), g.addressMap.addressOf(binding), g.addressMap.addressOf(binding.Parent()))
		}
	}

	// Helper function that 'modify' the given memory bindings
	modifyMemoryBindingsData := func(pb *AtomBehaviour, bindings []*vulkanDeviceMemoryBinding) {
		for _, binding := range bindings {
			pb.modify(g, binding.data)
			debug("\tmodify binding data: %v <- binding: %v <- memory: %v", binding.data, g.addressMap.addressOf(binding.data), g.addressMap.addressOf(binding), g.addressMap.addressOf(binding.Parent()))
		}
	}

	// Helper function that adds 'read' to the given command buffer handle and
	// 'modify' to the given comamnd buffer records to the current behavior, if
	// such behaviours have not been added before. And records a callback to
	// carry out other behaviours later when the command buffer is submitted.
	recordCommand := func(currentBehaviour *AtomBehaviour,
		handle VkCommandBuffer,
		c func(futureBehaviour *AtomBehaviour)) {
		cmdBuf := g.getOrCreateCommandBuffer(handle)
		if len(currentBehaviour.Read) == 0 || currentBehaviour.Read[len(currentBehaviour.Read)-1] !=
			g.addressMap.addressOf(cmdBuf.handle) {
			currentBehaviour.read(g, cmdBuf.handle)
		}
		if len(currentBehaviour.Modify) == 0 || currentBehaviour.Modify[len(currentBehaviour.Modify)-1] !=
			g.addressMap.addressOf(cmdBuf.records) {
			currentBehaviour.modify(g, cmdBuf.records)
		}

		cmdBuf.records.appendCommand(c)
	}

	// Helper function that adds 'read' to the given command buffer handle and
	// 'modify' to the given comamnd buffer records to the current behavior, if
	// such behaviours have not been added before. And records 'read' of the
	// given read memory bindings, 'modify' of the given modify memory bindings
	// and 'write' of the given write memory bindings, to be carried out later
	// when the command buffer is submitted.
	recordTouchingMemoryBindingsData := func(currentBehaviour *AtomBehaviour,
		handle VkCommandBuffer,
		readBindings, modifyBindings, writeBindings []*vulkanDeviceMemoryBinding) {
		cmdBuf := g.getOrCreateCommandBuffer(handle)
		if len(currentBehaviour.Read) == 0 || currentBehaviour.Read[len(currentBehaviour.Read)-1] !=
			g.addressMap.addressOf(cmdBuf.handle) {
			currentBehaviour.read(g, cmdBuf.handle)
		}
		if len(currentBehaviour.Modify) == 0 || currentBehaviour.Modify[len(currentBehaviour.Modify)-1] !=
			g.addressMap.addressOf(cmdBuf.records) {
			currentBehaviour.modify(g, cmdBuf.records)
		}

		cmdBuf.records.appendCommand(func(b *AtomBehaviour) {
			readMemoryBindingsData(b, readBindings)
			modifyMemoryBindingsData(b, modifyBindings)
			writeMemoryBindingsData(b, writeBindings)
		})
	}

	// Mutate the state with the atom.
	if err := a.Mutate(ctx, s, nil); err != nil {
		log.E(ctx, "Atom %v %v: %v", id, a, err)
		return AtomBehaviour{Aborted: true}
	}

	debug("DCE::DependencyGraph::getBehaviour: %v, %v", id, reflect.TypeOf(a))

	// Add behaviors for the atom according to its type.
	// Note that there are a few cases in which the behaviour is NOT added to the
	// place that the behaviour is carried out in real execution of the API
	// commands:
	// Draw commands (vkCmdDraw, RecreateCmdDraw, vkCmdDrawIndexed, etc):
	// The 'read' behaviour of the currently bound vertex buffer and index
	// buffers are recorded to the command buffer records by binding commands,
	// like: vkCmdBindVertexBuffers etc, not by the draw commands. This is
	// because after the call to vkQueueSubmit's Mutate(), when we process the
	// recorded draw command, only the last set of bound vertex buffers and
	// bound index buffer will be kept in the global's state
	// CurrentBoundVertexBuffers. So we cannot obtain previous bound vertex
	// buffers from it and so we cannot add 'read' behaviours to the buffers
	// data. To solve the problem, we read the buffer memory data here. This may
	// result into a dummy read behavior of the buffer data, as the buffer may
	// never be used later. But this ensures the correctness of the trace and the
	// state.
	// 'Read' and 'modify' behaviours to descriptors, like textures, uniform
	// buffers, etc, have similar problem, as we cannot application is allowed
	// to call vkCmdBindDescriptorSets multiple times and we only get the last
	// bound one after VkQueueSubmit's Mutate() is called. So we records the
	// behaviours in VkCmdBindDescriptorSets and RecreateCmdBindDescriptorSets,
	// instead of the draw calls.
	switch a := a.(type) {
	case *VkCreateImage:
		image := a.PImage.Read(ctx, a, s, nil)
		addWrite(&b, g, vulkanStateKey(image))

	case *VkCreateBuffer:
		buffer := a.PBuffer.Read(ctx, a, s, nil)
		addWrite(&b, g, vulkanStateKey(buffer))

	case *RecreateImage:
		image := a.PImage.Read(ctx, a, s, nil)
		addWrite(&b, g, vulkanStateKey(image))

	case *RecreateBuffer:
		buffer := a.PBuffer.Read(ctx, a, s, nil)
		addWrite(&b, g, vulkanStateKey(buffer))

	case *VkAllocateMemory:
		allocateInfo := a.PAllocateInfo.Read(ctx, a, s, nil)
		memory := a.PMemory.Read(ctx, a, s, nil)
		addWrite(&b, g, g.getOrCreateDeviceMemory(memory))

		// handle dedicated memory allocation
		if allocateInfo.PNext != (Voidᶜᵖ{}) {
			pNext := Voidᵖ(allocateInfo.PNext)
			for pNext != (Voidᵖ{}) {
				sType := (VkStructureTypeᶜᵖ(pNext)).Read(ctx, a, s, nil)
				switch sType {
				case VkStructureType_VK_STRUCTURE_TYPE_DEDICATED_ALLOCATION_MEMORY_ALLOCATE_INFO_NV:
					ext := VkDedicatedAllocationMemoryAllocateInfoNVᵖ(pNext).Read(ctx, a, s, nil)
					image := ext.Image
					buffer := ext.Buffer
					if uint64(image) != 0 {
						addRead(&b, g, vulkanStateKey(image))
					}
					if uint64(buffer) != 0 {
						addRead(&b, g, vulkanStateKey(buffer))
					}
				}
				pNext = (VulkanStructHeaderᵖ(pNext)).Read(ctx, a, s, nil).PNext
			}
		}

	case *RecreateDeviceMemory:
		allocateInfo := a.PAllocateInfo.Read(ctx, a, s, nil)
		memory := a.PMemory.Read(ctx, a, s, nil)
		addWrite(&b, g, g.getOrCreateDeviceMemory(memory))

		// handle dedicated memory allocation
		if allocateInfo.PNext != (Voidᶜᵖ{}) {
			pNext := Voidᵖ(allocateInfo.PNext)
			for pNext != (Voidᵖ{}) {
				sType := (VkStructureTypeᶜᵖ(pNext)).Read(ctx, a, s, nil)
				switch sType {
				case VkStructureType_VK_STRUCTURE_TYPE_DEDICATED_ALLOCATION_MEMORY_ALLOCATE_INFO_NV:
					ext := VkDedicatedAllocationMemoryAllocateInfoNVᵖ(pNext).Read(ctx, a, s, nil)
					image := ext.Image
					buffer := ext.Buffer
					if uint64(image) != 0 {
						addRead(&b, g, vulkanStateKey(image))
					}
					if uint64(buffer) != 0 {
						addRead(&b, g, vulkanStateKey(buffer))
					}
				}
				pNext = (VulkanStructHeaderᵖ(pNext)).Read(ctx, a, s, nil).PNext
			}
		}

	case *VkBindImageMemory:
		image := a.Image
		memory := a.Memory
		addModify(&b, g, vulkanStateKey(image))
		addRead(&b, g, g.getOrCreateDeviceMemory(memory).handle)
		if GetState(s).Images.Contains(image) {
			offset := uint64(GetState(s).Images.Get(image).BoundMemoryOffset)
			// In some applications, `vkGetImageMemoryRequirements` is not called so we
			// don't have the image size. However, a memory binding for a zero-sized
			// memory range will also be created here and used later to check
			// overlapping. The problem is that this memory range will always be
			// considered as fully covered by any range that starts at the same offset
			// or across the offset.
			// So to ensure correctness, overwriting of zero sized memory binding is
			// not allowed, execept for the vkCmdBeginRenderPass, whose target is
			// always an image as a whole.
			// TODO(qining) Fix this
			size := uint64(GetState(s).Images.Get(image).Size)
			binding := g.getOrCreateDeviceMemory(memory).addBinding(offset, size)
			addWrite(&b, g, binding)
		}

	case *VkBindBufferMemory:
		buffer := a.Buffer
		memory := a.Memory
		addModify(&b, g, vulkanStateKey(buffer))
		addRead(&b, g, g.getOrCreateDeviceMemory(memory).handle)
		if GetState(s).Buffers.Contains(buffer) {
			offset := uint64(GetState(s).Buffers.Get(buffer).MemoryOffset)
			size := uint64(GetState(s).Buffers.Get(buffer).Info.Size)
			binding := g.getOrCreateDeviceMemory(memory).addBinding(offset, size)
			addWrite(&b, g, binding)
		}

	case *RecreateBindImageMemory:
		image := a.Image
		memory := a.Memory
		addModify(&b, g, vulkanStateKey(image))
		addRead(&b, g, g.getOrCreateDeviceMemory(memory).handle)
		if GetState(s).Images.Contains(image) {
			offset := uint64(GetState(s).Images.Get(image).BoundMemoryOffset)
			size := uint64(GetState(s).Images.Get(image).Size)
			binding := g.getOrCreateDeviceMemory(memory).addBinding(offset, size)
			addWrite(&b, g, binding)
		}

	case *RecreateBindBufferMemory:
		buffer := a.Buffer
		memory := a.Memory
		addModify(&b, g, vulkanStateKey(buffer))
		addRead(&b, g, g.getOrCreateDeviceMemory(memory).handle)
		if GetState(s).Buffers.Contains(buffer) {
			offset := uint64(GetState(s).Buffers.Get(buffer).MemoryOffset)
			size := uint64(GetState(s).Buffers.Get(buffer).Info.Size)
			binding := g.getOrCreateDeviceMemory(memory).addBinding(offset, size)
			addWrite(&b, g, binding)
		}

	case *RecreateImageData:
		image := a.Image
		addModify(&b, g, vulkanStateKey(image))
		overlappingBindings := getOverlappedBindingsForImage(image)
		writeMemoryBindingsData(&b, overlappingBindings)

	case *RecreateBufferData:
		buffer := a.Buffer
		addModify(&b, g, vulkanStateKey(buffer))
		overlappingBindings := getOverlappedBindingsForBuffer(buffer)
		writeMemoryBindingsData(&b, overlappingBindings)

	case *VkDestroyImage:
		image := a.Image
		addModify(&b, g, vulkanStateKey(image))
		b.KeepAlive = true

	case *VkDestroyBuffer:
		buffer := a.Buffer
		addModify(&b, g, vulkanStateKey(buffer))
		b.KeepAlive = true

	case *VkFreeMemory:
		memory := a.Memory
		// Free/deletion atoms are kept alive so the creation atom of the
		// corresponding handle will also be kept alive, even though the handle
		// may not be used anywhere else.
		addRead(&b, g, vulkanStateKey(memory))
		b.KeepAlive = true

	case *VkMapMemory:
		memory := a.Memory
		addModify(&b, g, g.getOrCreateDeviceMemory(memory))

	case *VkUnmapMemory:
		memory := a.Memory
		addModify(&b, g, g.getOrCreateDeviceMemory(memory))

	case *VkFlushMappedMemoryRanges:
		ranges := a.PMemoryRanges.Slice(0, uint64(a.MemoryRangeCount), s)
		// TODO: Link the contiguous ranges into one so that we don't miss
		// potential overwrites
		for i := uint64(0); i < uint64(a.MemoryRangeCount); i++ {
			mappedRange := ranges.Index(i, s).Read(ctx, a, s, nil)
			memory := mappedRange.Memory
			offset := uint64(mappedRange.Offset)
			size := uint64(mappedRange.Size)
			// For the overlapping bindings in the memory, if the flush range covers
			// the whole binding range, the data in that binding will be overwritten,
			// otherwise the data is modified.
			bindings := getOverlappingMemoryBindings(memory, offset, size)
			for _, binding := range bindings {
				if offset <= binding.start && offset+size >= binding.end {
					// If the memory binding size is zero, the binding is for an image
					// whose size is unknown at binding time. As we don't know whether
					// this flush overwrites the whole image, we conservatively label the
					// flushing always as 'modify'
					if binding.start == binding.end {
						addModify(&b, g, binding.data)
					} else {
						addWrite(&b, g, binding.data)
					}
				} else {
					addModify(&b, g, binding.data)
				}
			}
		}

	case *VkInvalidateMappedMemoryRanges:
		ranges := a.PMemoryRanges.Slice(0, uint64(a.MemoryRangeCount), s)
		// TODO: Link the contiguous ranges
		for i := uint64(0); i < uint64(a.MemoryRangeCount); i++ {
			mappedRange := ranges.Index(i, s).Read(ctx, a, s, nil)
			memory := mappedRange.Memory
			offset := uint64(mappedRange.Offset)
			size := uint64(mappedRange.Size)
			bindings := getOverlappingMemoryBindings(memory, offset, size)
			readMemoryBindingsData(&b, bindings)
		}

	case *VkCreateImageView:
		createInfo := a.PCreateInfo.Read(ctx, a, s, nil)
		image := createInfo.Image
		view := a.PView.Read(ctx, a, s, nil)
		addRead(&b, g, vulkanStateKey(image))
		addWrite(&b, g, vulkanStateKey(view))

	case *RecreateImageView:
		createInfo := a.PCreateInfo.Read(ctx, a, s, nil)
		image := createInfo.Image
		view := a.PImageView.Read(ctx, a, s, nil)
		addRead(&b, g, vulkanStateKey(image))
		addWrite(&b, g, vulkanStateKey(view))

	case *VkCreateBufferView:
		createInfo := a.PCreateInfo.Read(ctx, a, s, nil)
		buffer := createInfo.Buffer
		view := a.PView.Read(ctx, a, s, nil)
		addRead(&b, g, vulkanStateKey(buffer))
		addWrite(&b, g, vulkanStateKey(view))

	case *RecreateBufferView:
		createInfo := a.PCreateInfo.Read(ctx, a, s, nil)
		buffer := createInfo.Buffer
		view := a.PBufferView.Read(ctx, a, s, nil)
		addRead(&b, g, vulkanStateKey(buffer))
		addWrite(&b, g, vulkanStateKey(view))

	case *VkUpdateDescriptorSets:
		// handle descriptor writes
		writeCount := a.DescriptorWriteCount
		if writeCount > 0 {
			writes := a.PDescriptorWrites.Slice(0, uint64(writeCount), s)
			if err := processDescriptorWrites(writes, &b, g, ctx, a, s); err != nil {
				log.E(ctx, "Atom %v %v: %v", id, a, err)
				return AtomBehaviour{Aborted: true}
			}
		}
		// handle descriptor copies
		copyCount := a.DescriptorCopyCount
		if copyCount > 0 {
			copies := a.PDescriptorCopies.Slice(0, uint64(copyCount), s)
			for i := uint32(0); i < copyCount; i++ {
				copy := copies.Index(uint64(i), s).Read(ctx, a, s, nil)
				srcDescriptor := copy.SrcSet
				dstDescriptor := copy.DstSet
				addRead(&b, g, vulkanStateKey(srcDescriptor))
				addModify(&b, g, vulkanStateKey(dstDescriptor))
			}
		}

	case *RecreateDescriptorSet:
		// handle descriptor writes
		writeCount := a.DescriptorWriteCount
		if writeCount > 0 {
			writes := a.PDescriptorWrites.Slice(0, uint64(writeCount), s)
			if err := processDescriptorWrites(writes, &b, g, ctx, a, s); err != nil {
				log.E(ctx, "Atom %v %v: %v", id, a, err)
				return AtomBehaviour{Aborted: true}
			}
		}

	case *VkCreateFramebuffer:
		addWrite(&b, g, vulkanStateKey(a.PFramebuffer.Read(ctx, a, s, nil)))
		addRead(&b, g, vulkanStateKey(a.PCreateInfo.Read(ctx, a, s, nil).RenderPass))
		// process the attachments
		createInfo := a.PCreateInfo.Read(ctx, a, s, nil)
		attachmentCount := createInfo.AttachmentCount
		attachments := createInfo.PAttachments.Slice(0, uint64(attachmentCount), s)
		for i := uint32(0); i < attachmentCount; i++ {
			attachedViews := attachments.Index(uint64(i), s).Read(ctx, a, s, nil)
			addRead(&b, g, vulkanStateKey(attachedViews))
		}

	case *RecreateFramebuffer:
		addWrite(&b, g, vulkanStateKey(a.PFramebuffer.Read(ctx, a, s, nil)))
		addRead(&b, g, vulkanStateKey(a.PCreateInfo.Read(ctx, a, s, nil).RenderPass))
		// process the attachments
		createInfo := a.PCreateInfo.Read(ctx, a, s, nil)
		attachmentCount := createInfo.AttachmentCount
		attachments := createInfo.PAttachments.Slice(0, uint64(attachmentCount), s)
		for i := uint32(0); i < attachmentCount; i++ {
			attachedViews := attachments.Index(uint64(i), s).Read(ctx, a, s, nil)
			addRead(&b, g, vulkanStateKey(attachedViews))
		}

	case *VkCreateRenderPass:
		addWrite(&b, g, vulkanStateKey(a.PRenderPass.Read(ctx, a, s, nil)))

	case *RecreateRenderPass:
		addWrite(&b, g, vulkanStateKey(a.PRenderPass.Read(ctx, a, s, nil)))

	case *VkCreateGraphicsPipelines:
		pipelineCount := uint64(a.CreateInfoCount)
		createInfos := a.PCreateInfos.Slice(0, pipelineCount, s)
		pipelines := a.PPipelines.Slice(0, pipelineCount, s)
		for i := uint64(0); i < pipelineCount; i++ {
			// read shaders
			stageCount := uint64(createInfos.Index(i, s).Read(ctx, a, s, nil).StageCount)
			shaderStages := createInfos.Index(i, s).Read(ctx, a, s, nil).PStages.Slice(0, stageCount, s)
			for j := uint64(0); j < stageCount; j++ {
				shaderStage := shaderStages.Index(j, s).Read(ctx, a, s, nil)
				module := shaderStage.Module
				addRead(&b, g, vulkanStateKey(module))
			}
			// read renderpass
			renderPass := createInfos.Index(i, s).Read(ctx, a, s, nil).RenderPass
			addRead(&b, g, vulkanStateKey(renderPass))
			// Create pipeline
			pipeline := pipelines.Index(i, s).Read(ctx, a, s, nil)
			addWrite(&b, g, vulkanStateKey(pipeline))
		}

	case *RecreateGraphicsPipeline:
		createInfo := a.PCreateInfo.Read(ctx, a, s, nil)
		stageCount := uint64(createInfo.StageCount)
		shaderStages := createInfo.PStages.Slice(0, stageCount, s)
		for i := uint64(0); i < stageCount; i++ {
			shaderStage := shaderStages.Index(i, s).Read(ctx, a, s, nil)
			addRead(&b, g, vulkanStateKey(shaderStage.Module))
		}
		addRead(&b, g, vulkanStateKey(createInfo.RenderPass))
		addWrite(&b, g, vulkanStateKey(a.PPipeline.Read(ctx, a, s, nil)))

	case *VkCreateComputePipelines:
		pipelineCount := uint64(a.CreateInfoCount)
		createInfos := a.PCreateInfos.Slice(0, pipelineCount, s)
		pipelines := a.PPipelines.Slice(0, pipelineCount, s)
		for i := uint64(0); i < pipelineCount; i++ {
			// read shader
			shaderStage := createInfos.Index(i, s).Read(ctx, a, s, nil).Stage
			module := shaderStage.Module
			addRead(&b, g, vulkanStateKey(module))
			// Create pipeline
			pipeline := pipelines.Index(i, s).Read(ctx, a, s, nil)
			addWrite(&b, g, vulkanStateKey(pipeline))
		}

	case *RecreateComputePipeline:
		createInfo := a.PCreateInfo.Read(ctx, a, s, nil)
		module := createInfo.Stage.Module
		addRead(&b, g, vulkanStateKey(module))
		addWrite(&b, g, vulkanStateKey(a.PPipeline.Read(ctx, a, s, nil)))

	case *VkCreateShaderModule:
		addWrite(&b, g, vulkanStateKey(a.PShaderModule.Read(ctx, a, s, nil)))

	case *RecreateShaderModule:
		addWrite(&b, g, vulkanStateKey(a.PShaderModule.Read(ctx, a, s, nil)))

	case *VkCmdCopyImage:
		srcBindings := readImageHandleAndGetBindings(&b, a.SrcImage)
		dstBindings := readImageHandleAndGetBindings(&b, a.DstImage)
		// Be conservative here. Without tracking all the memory ranges and
		// calculating the memory according to the copy region, we cannot assume
		// this command overwrites the data. So it is labelled as 'modify' to
		// kept the previous writes
		// TODO(qining): Track all the memory ranges
		recordTouchingMemoryBindingsData(&b, a.CommandBuffer, srcBindings,
			dstBindings, emptyMemoryBindings)

	case *RecreateCmdCopyImage:
		srcBindings := readImageHandleAndGetBindings(&b, a.SrcImage)
		dstBindings := readImageHandleAndGetBindings(&b, a.DstImage)
		// Be conservative here. Without tracking all the memory ranges and
		// calculating the memory according to the copy region, we cannot assume
		// this command overwrites the data. So it is labelled as 'modify' to
		// kept the previous writes
		// TODO(qining): Track all the memory ranges
		recordTouchingMemoryBindingsData(&b, a.CommandBuffer, srcBindings,
			dstBindings, emptyMemoryBindings)

	case *VkCmdCopyImageToBuffer:
		srcBindings := readImageHandleAndGetBindings(&b, a.SrcImage)
		dstBindings := readBufferHandleAndGetBindings(&b, a.DstBuffer)
		// Be conservative here. Without tracking all the memory ranges and
		// calculating the memory according to the copy region, we cannot assume
		// this command overwrites the data. So it is labelled as 'modify' to
		// kept the previous writes
		recordTouchingMemoryBindingsData(&b, a.CommandBuffer,
			srcBindings, dstBindings, emptyMemoryBindings)

	case *RecreateCmdCopyImageToBuffer:
		srcBindings := readImageHandleAndGetBindings(&b, a.SrcImage)
		dstBindings := readBufferHandleAndGetBindings(&b, a.DstBuffer)
		// Be conservative here. Without tracking all the memory ranges and
		// calculating the memory according to the copy region, we cannot assume
		// this command overwrites the data. So it is labelled as 'modify' to
		// kept the previous writes
		recordTouchingMemoryBindingsData(&b, a.CommandBuffer,
			srcBindings, dstBindings, emptyMemoryBindings)

	case *VkCmdCopyBufferToImage:
		srcBindings := readBufferHandleAndGetBindings(&b, a.SrcBuffer)
		dstBindings := readImageHandleAndGetBindings(&b, a.DstImage)
		// Be conservative here. Without tracking all the memory ranges and
		// calculating the memory according to the copy region, we cannot assume
		// this command overwrites the data. So it is labelled as 'modify' to
		// kept the previous writes
		recordTouchingMemoryBindingsData(&b, a.CommandBuffer,
			srcBindings, dstBindings, emptyMemoryBindings)

	case *RecreateCmdCopyBufferToImage:
		srcBindings := readBufferHandleAndGetBindings(&b, a.SrcBuffer)
		dstBindings := readImageHandleAndGetBindings(&b, a.DstImage)
		// Be conservative here. Without tracking all the memory ranges and
		// calculating the memory according to the copy region, we cannot assume
		// this command overwrites the data. So it is labelled as 'modify' to
		// kept the previous writes
		recordTouchingMemoryBindingsData(&b, a.CommandBuffer,
			srcBindings, dstBindings, emptyMemoryBindings)

	case *VkCmdCopyBuffer:
		srcBindings := readBufferHandleAndGetBindings(&b, a.SrcBuffer)
		dstBindings := readBufferHandleAndGetBindings(&b, a.DstBuffer)
		// Be conservative here. Without tracking all the memory ranges and
		// calculating the memory according to the copy region, we cannot assume
		// this command overwrites the data. So it is labelled as 'modify' to
		// kept the previous writes
		recordTouchingMemoryBindingsData(&b, a.CommandBuffer,
			srcBindings, dstBindings, emptyMemoryBindings)

	case *RecreateCmdCopyBuffer:
		srcBindings := readBufferHandleAndGetBindings(&b, a.SrcBuffer)
		dstBindings := readBufferHandleAndGetBindings(&b, a.DstBuffer)
		// Be conservative here. Without tracking all the memory ranges and
		// calculating the memory according to the copy region, we cannot assume
		// this command overwrites the data. So it is labelled as 'modify' to
		// kept the previous writes
		recordTouchingMemoryBindingsData(&b, a.CommandBuffer,
			srcBindings, dstBindings, emptyMemoryBindings)

	case *VkCmdBlitImage:
		srcBindings := readImageHandleAndGetBindings(&b, a.SrcImage)
		dstBindings := readImageHandleAndGetBindings(&b, a.DstImage)
		// Be conservative here. Without tracking all the memory ranges and
		// calculating the memory according to the copy region, we cannot assume
		// this command overwrites the data. So it is labelled as 'modify' to
		// kept the previous writes
		// TODO(qining): Track all the memory ranges
		recordTouchingMemoryBindingsData(&b, a.CommandBuffer, srcBindings,
			dstBindings, emptyMemoryBindings)

	case *RecreateCmdBlitImage:
		srcBindings := readImageHandleAndGetBindings(&b, a.SrcImage)
		dstBindings := readImageHandleAndGetBindings(&b, a.DstImage)
		// Be conservative here. Without tracking all the memory ranges and
		// calculating the memory according to the copy region, we cannot assume
		// this command overwrites the data. So it is labelled as 'modify' to
		// kept the previous writes
		// TODO(qining): Track all the memory ranges
		recordTouchingMemoryBindingsData(&b, a.CommandBuffer, srcBindings,
			dstBindings, emptyMemoryBindings)

	case *VkCmdResolveImage:
		srcBindings := readImageHandleAndGetBindings(&b, a.SrcImage)
		dstBindings := readImageHandleAndGetBindings(&b, a.DstImage)
		// Be conservative here. Without tracking all the memory ranges and
		// calculating the memory according to the copy region, we cannot assume
		// this command overwrites the data. So it is labelled as 'modify' to
		// kept the previous writes
		// TODO(qining): Track all the memory ranges
		recordTouchingMemoryBindingsData(&b, a.CommandBuffer, srcBindings,
			dstBindings, emptyMemoryBindings)

	case *RecreateCmdResolveImage:
		srcBindings := readImageHandleAndGetBindings(&b, a.SrcImage)
		dstBindings := readImageHandleAndGetBindings(&b, a.DstImage)
		// Be conservative here. Without tracking all the memory ranges and
		// calculating the memory according to the copy region, we cannot assume
		// this command overwrites the data. So it is labelled as 'modify' to
		// kept the previous writes
		// TODO(qining): Track all the memory ranges
		recordTouchingMemoryBindingsData(&b, a.CommandBuffer, srcBindings,
			dstBindings, emptyMemoryBindings)

	case *VkCmdFillBuffer:
		dstBindings := readBufferHandleAndGetBindings(&b, a.DstBuffer)
		// Be conservative here. Without tracking all the memory ranges and
		// calculating the memory according to the copy region, we cannot assume
		// this command overwrites the data. So it is labelled as 'modify' to
		// kept the previous writes
		recordTouchingMemoryBindingsData(&b, a.CommandBuffer, emptyMemoryBindings,
			dstBindings, emptyMemoryBindings)

	case *RecreateCmdFillBuffer:
		dstBindings := readBufferHandleAndGetBindings(&b, a.DstBuffer)
		// Be conservative here. Without tracking all the memory ranges and
		// calculating the memory according to the copy region, we cannot assume
		// this command overwrites the data. So it is labelled as 'modify' to
		// kept the previous writes
		recordTouchingMemoryBindingsData(&b, a.CommandBuffer, emptyMemoryBindings,
			dstBindings, emptyMemoryBindings)

	case *VkCmdUpdateBuffer:
		dstBindings := readBufferHandleAndGetBindings(&b, a.DstBuffer)
		// Be conservative here. Without tracking all the memory ranges and
		// calculating the memory according to the copy region, we cannot assume
		// this command overwrites the data. So it is labelled as 'modify' to
		// kept the previous writes
		recordTouchingMemoryBindingsData(&b, a.CommandBuffer, emptyMemoryBindings,
			dstBindings, emptyMemoryBindings)

	case *RecreateCmdUpdateBuffer:
		dstBindings := readBufferHandleAndGetBindings(&b, a.DstBuffer)
		// Be conservative here. Without tracking all the memory ranges and
		// calculating the memory according to the copy region, we cannot assume
		// this command overwrites the data. So it is labelled as 'modify' to
		// kept the previous writes
		recordTouchingMemoryBindingsData(&b, a.CommandBuffer, emptyMemoryBindings,
			dstBindings, emptyMemoryBindings)

	case *VkCmdCopyQueryPoolResults:
		dstBindings := readBufferHandleAndGetBindings(&b, a.DstBuffer)
		// Be conservative here. Without tracking all the memory ranges and
		// calculating the memory according to the copy region, we cannot assume
		// this command overwrites the data. So it is labelled as 'modify' to
		// kept the previous writes
		recordTouchingMemoryBindingsData(&b, a.CommandBuffer, emptyMemoryBindings,
			dstBindings, emptyMemoryBindings)

	case *RecreateCmdCopyQueryPoolResults:
		dstBindings := readBufferHandleAndGetBindings(&b, a.DstBuffer)
		// Be conservative here. Without tracking all the memory ranges and
		// calculating the memory according to the copy region, we cannot assume
		// this command overwrites the data. So it is labelled as 'modify' to
		// kept the previous writes
		recordTouchingMemoryBindingsData(&b, a.CommandBuffer, emptyMemoryBindings,
			dstBindings, emptyMemoryBindings)

	case *VkCmdBindVertexBuffers:
		count := a.BindingCount
		buffers := a.PBuffers.Slice(0, uint64(count), s)
		for i := uint64(0); i < uint64(count); i++ {
			buffer := buffers.Index(i, s).Read(ctx, a, s, nil)
			bufferBindings := readBufferHandleAndGetBindings(&b, buffer)
			recordCommand(&b, a.CommandBuffer, func(b *AtomBehaviour) {
				// As the LastBoundQueue of the buffer object has will change, so it is
				// a 'modify' instead of a 'read'
				addModify(b, g, vulkanStateKey(buffer))
				// Read the vertex buffer memory data here.
				readMemoryBindingsData(b, bufferBindings)
			})
		}

	case *RecreateCmdBindVertexBuffers:
		count := a.BindingCount
		buffers := a.PBuffers.Slice(0, uint64(count), s)
		for i := uint64(0); i < uint64(count); i++ {
			buffer := buffers.Index(i, s).Read(ctx, a, s, nil)
			bufferBindings := readBufferHandleAndGetBindings(&b, buffer)
			recordCommand(&b, a.CommandBuffer, func(b *AtomBehaviour) {
				// As the LastBoundQueue of the buffer object has will change, so it is
				// a 'modify' instead of a 'read'
				addModify(b, g, vulkanStateKey(buffer))
				// Read the vertex buffer memory data here.
				readMemoryBindingsData(b, bufferBindings)
			})
		}

	case *VkCmdBindIndexBuffer:
		buffer := a.Buffer
		bufferBindings := readBufferHandleAndGetBindings(&b, buffer)
		recordCommand(&b, a.CommandBuffer, func(b *AtomBehaviour) {
			// As the LastBoundQueue of the buffer object has will change, so it is
			// a 'modify' instead of a 'read'
			addModify(b, g, vulkanStateKey(buffer))
			// Read the index buffer memory data here.
			readMemoryBindingsData(b, bufferBindings)
		})

	case *RecreateCmdBindIndexBuffer:
		buffer := a.Buffer
		bufferBindings := readBufferHandleAndGetBindings(&b, buffer)
		recordCommand(&b, a.CommandBuffer, func(b *AtomBehaviour) {
			// As the LastBoundQueue of the buffer object has will change, so it is
			// a 'modify' instead of a 'read'
			addModify(b, g, vulkanStateKey(buffer))
			// Read the index buffer memory data here.
			readMemoryBindingsData(b, bufferBindings)
		})

	case *VkCmdDraw:
		recordCommand(&b, a.CommandBuffer, func(b *AtomBehaviour) {})

	case *RecreateCmdDraw:
		recordCommand(&b, a.CommandBuffer, func(b *AtomBehaviour) {})

	case *VkCmdDrawIndexed:
		recordCommand(&b, a.CommandBuffer, func(b *AtomBehaviour) {})

	case *RecreateCmdDrawIndexed:
		recordCommand(&b, a.CommandBuffer, func(b *AtomBehaviour) {})

	case *VkCmdDrawIndirect:
		indirectBuf := a.Buffer
		bufferBindings := readBufferHandleAndGetBindings(&b, indirectBuf)
		recordTouchingMemoryBindingsData(&b, a.CommandBuffer,
			bufferBindings, emptyMemoryBindings, emptyMemoryBindings)

	case *RecreateCmdDrawIndirect:
		indirectBuf := a.Buffer
		bufferBindings := readBufferHandleAndGetBindings(&b, indirectBuf)
		recordTouchingMemoryBindingsData(&b, a.CommandBuffer,
			bufferBindings, emptyMemoryBindings, emptyMemoryBindings)

	case *VkCmdDrawIndexedIndirect:
		indirectBuf := a.Buffer
		bufferBindings := readBufferHandleAndGetBindings(&b, indirectBuf)
		recordTouchingMemoryBindingsData(&b, a.CommandBuffer,
			bufferBindings, emptyMemoryBindings, emptyMemoryBindings)

	case *RecreateCmdDrawIndexedIndirect:
		indirectBuf := a.Buffer
		bufferBindings := readBufferHandleAndGetBindings(&b, indirectBuf)
		recordTouchingMemoryBindingsData(&b, a.CommandBuffer,
			bufferBindings, emptyMemoryBindings, emptyMemoryBindings)

	case *VkCmdDispatch:
		recordCommand(&b, a.CommandBuffer, func(b *AtomBehaviour) {})

	case *RecreateCmdDispatch:
		recordCommand(&b, a.CommandBuffer, func(b *AtomBehaviour) {})

	case *VkCmdDispatchIndirect:
		buffer := a.Buffer
		bufferBindings := readBufferHandleAndGetBindings(&b, buffer)
		recordTouchingMemoryBindingsData(&b, a.CommandBuffer,
			bufferBindings, emptyMemoryBindings, emptyMemoryBindings)

	case *RecreateCmdDispatchIndirect:
		buffer := a.Buffer
		bufferBindings := readBufferHandleAndGetBindings(&b, buffer)
		recordTouchingMemoryBindingsData(&b, a.CommandBuffer,
			bufferBindings, emptyMemoryBindings, emptyMemoryBindings)

	case *VkCmdBeginRenderPass:
		beginInfo := a.PRenderPassBegin.Read(ctx, a, s, nil)
		framebuffer := beginInfo.Framebuffer
		addRead(&b, g, vulkanStateKey(framebuffer))
		renderpass := beginInfo.RenderPass
		addRead(&b, g, vulkanStateKey(renderpass))

		if GetState(s).Framebuffers.Contains(framebuffer) {
			atts := GetState(s).Framebuffers.Get(framebuffer).ImageAttachments
			if GetState(s).RenderPasses.Contains(renderpass) {
				attDescs := GetState(s).RenderPasses.Get(renderpass).AttachmentDescriptions
				for i := uint32(0); i < uint32(len(atts)); i++ {
					img := atts.Get(i).Image.VulkanHandle
					// This can be wrong as this is getting all the memory bindings
					// that OVERLAP with the attachment image, so extra memories might be
					// covered. However in practical, image should be bound to only one
					// memory binding as a whole. So here should be a problem.
					// TODO: Use intersection operation to get the memory ranges
					imgBindings := getOverlappedBindingsForImage(img)
					loadOp := attDescs.Get(i).LoadOp
					storeOp := attDescs.Get(i).StoreOp

					if (loadOp != VkAttachmentLoadOp_VK_ATTACHMENT_LOAD_OP_LOAD) &&
						(storeOp != VkAttachmentStoreOp_VK_ATTACHMENT_STORE_OP_DONT_CARE) {
						// If the loadOp is not LOAD, and the storeOp is not DONT_CARE, the
						// render target attachment's data should be overwritten later.
						recordTouchingMemoryBindingsData(&b, a.CommandBuffer,
							emptyMemoryBindings, emptyMemoryBindings, imgBindings)
					} else if (loadOp == VkAttachmentLoadOp_VK_ATTACHMENT_LOAD_OP_LOAD) &&
						(storeOp != VkAttachmentStoreOp_VK_ATTACHMENT_STORE_OP_DONT_CARE) {
						// If the loadOp is LOAD, and the storeOp is not DONT_CARE, the
						// render target attachment should be 'modified'.
						recordTouchingMemoryBindingsData(&b, a.CommandBuffer,
							emptyMemoryBindings, imgBindings, emptyMemoryBindings)
					} else if (loadOp == VkAttachmentLoadOp_VK_ATTACHMENT_LOAD_OP_LOAD) &&
						(storeOp == VkAttachmentStoreOp_VK_ATTACHMENT_STORE_OP_DONT_CARE) {
						// If the storeOp is DONT_CARE, and the loadOp is LOAD, the render target
						// attachment should be 'read'.
						recordTouchingMemoryBindingsData(&b, a.CommandBuffer, imgBindings,
							emptyMemoryBindings, emptyMemoryBindings)
					}
					// If the LoadOp is not LOAD and the storeOp is DONT_CARE, no operation
					// must be done to the attahcment then.
					// TODO(qining): Actually we should disable all the 'write', 'modify'
					// behaviour in this render pass.
				}
			}
		}

	case *RecreateCmdBeginRenderPass:

		beginInfo := a.PRenderPassBegin.Read(ctx, a, s, nil)
		framebuffer := beginInfo.Framebuffer
		addRead(&b, g, vulkanStateKey(framebuffer))
		renderpass := beginInfo.RenderPass
		addRead(&b, g, vulkanStateKey(renderpass))

		if GetState(s).Framebuffers.Contains(framebuffer) {
			atts := GetState(s).Framebuffers.Get(framebuffer).ImageAttachments
			if GetState(s).RenderPasses.Contains(renderpass) {
				attDescs := GetState(s).RenderPasses.Get(renderpass).AttachmentDescriptions
				for i := uint32(0); i < uint32(len(atts)); i++ {
					img := atts.Get(i).Image.VulkanHandle
					// This can be wrong as this is getting all the memory bindings
					// that OVERLAP with the attachment image, so extra memories might be
					// covered. However in practical, image should be bound to only one
					// memory binding as a whole. So here should be a problem.
					// TODO: Use intersection operation to get the memory ranges
					imgBindings := getOverlappedBindingsForImage(img)
					loadOp := attDescs.Get(i).LoadOp
					storeOp := attDescs.Get(i).StoreOp

					if (loadOp != VkAttachmentLoadOp_VK_ATTACHMENT_LOAD_OP_LOAD) &&
						(storeOp != VkAttachmentStoreOp_VK_ATTACHMENT_STORE_OP_DONT_CARE) {
						// If the loadOp is not LOAD, and the storeOp is not DONT_CARE, the
						// render target attachment's data should be overwritten later.
						recordTouchingMemoryBindingsData(&b, a.CommandBuffer,
							emptyMemoryBindings, emptyMemoryBindings, imgBindings)
					} else if (loadOp == VkAttachmentLoadOp_VK_ATTACHMENT_LOAD_OP_LOAD) &&
						(storeOp != VkAttachmentStoreOp_VK_ATTACHMENT_STORE_OP_DONT_CARE) {
						// If the loadOp is LOAD, and the storeOp is not DONT_CARE, the
						// render target attachment should be 'modified'.
						recordTouchingMemoryBindingsData(&b, a.CommandBuffer,
							emptyMemoryBindings, imgBindings, emptyMemoryBindings)
					} else if (loadOp == VkAttachmentLoadOp_VK_ATTACHMENT_LOAD_OP_LOAD) &&
						(storeOp == VkAttachmentStoreOp_VK_ATTACHMENT_STORE_OP_DONT_CARE) {
						// If the storeOp is DONT_CARE, and the loadOp is LOAD, the render target
						// attachment should be 'read'.
						recordTouchingMemoryBindingsData(&b, a.CommandBuffer, imgBindings,
							emptyMemoryBindings, emptyMemoryBindings)
					}
					// If the LoadOp is not LOAD and the storeOp is DONT_CARE, no operation
					// must be done to the attahcment then.
					// TODO(qining): Actually we should disable all the 'write', 'modify'
					// behaviour in this render pass.
				}
			}
		}

	case *VkCmdEndRenderPass:
		recordCommand(&b, a.CommandBuffer, func(b *AtomBehaviour) {})

	case *RecreateCmdEndRenderPass:
		recordCommand(&b, a.CommandBuffer, func(b *AtomBehaviour) {})

	case *VkCmdNextSubpass:
		recordCommand(&b, a.CommandBuffer, func(b *AtomBehaviour) {})

	case *RecreateCmdNextSubpass:
		recordCommand(&b, a.CommandBuffer, func(b *AtomBehaviour) {})

	case *VkCmdPushConstants:
		recordCommand(&b, a.CommandBuffer, func(b *AtomBehaviour) {})

	case *RecreateCmdPushConstants:
		recordCommand(&b, a.CommandBuffer, func(b *AtomBehaviour) {})

	case *VkCmdSetLineWidth:
		recordCommand(&b, a.CommandBuffer, func(b *AtomBehaviour) {})

	case *RecreateCmdSetLineWidth:
		recordCommand(&b, a.CommandBuffer, func(b *AtomBehaviour) {})

	case *VkCmdSetScissor:
		recordCommand(&b, a.CommandBuffer, func(b *AtomBehaviour) {})

	case *RecreateCmdSetScissor:
		recordCommand(&b, a.CommandBuffer, func(b *AtomBehaviour) {})

	case *VkCmdSetViewport:
		recordCommand(&b, a.CommandBuffer, func(b *AtomBehaviour) {})

	case *RecreateCmdSetViewport:
		recordCommand(&b, a.CommandBuffer, func(b *AtomBehaviour) {})

	case *VkCmdBindDescriptorSets:
		descriptorSetCount := a.DescriptorSetCount
		descriptorSets := a.PDescriptorSets.Slice(0, uint64(descriptorSetCount), s)
		for i := uint32(0); i < descriptorSetCount; i++ {
			descriptorSet := descriptorSets.Index(uint64(i), s).Read(ctx, a, s, nil)
			addRead(&b, g, vulkanStateKey(descriptorSet))
			if GetState(s).DescriptorSets.Contains(descriptorSet) {
				for _, descBinding := range GetState(s).DescriptorSets.Get(descriptorSet).Bindings {
					for _, bufferInfo := range descBinding.BufferBinding {
						buf := bufferInfo.Buffer

						recordCommand(&b, a.CommandBuffer, func(b *AtomBehaviour) {
							// Descriptors might be modified
							addModify(b, g, vulkanStateKey(buf))
							// Advance the read/modify behavior of the descriptors from
							// draw and dispatch calls to here. Details in the handling
							// of vkCmdDispatch and vkCmdDraw.
							modifyMemoryBindingsData(b, getOverlappedBindingsForBuffer(buf))
						})
					}
					for _, imageInfo := range descBinding.ImageBinding {
						view := imageInfo.ImageView

						recordCommand(&b, a.CommandBuffer, func(b *AtomBehaviour) {
							addRead(b, g, vulkanStateKey(view))
							if GetState(s).ImageViews.Contains(view) {
								img := GetState(s).ImageViews.Get(view).Image.VulkanHandle
								// Advance the read/modify behavior of the descriptors from
								// draw and dispatch calls to here. Details in the handling
								// of vkCmdDispatch and vkCmdDraw.
								readMemoryBindingsData(b, getOverlappedBindingsForImage(img))
							}
						})
					}
					for _, bufferView := range descBinding.BufferViewBindings {

						recordCommand(&b, a.CommandBuffer, func(b *AtomBehaviour) {
							addRead(b, g, vulkanStateKey(bufferView))
							if GetState(s).BufferViews.Contains(bufferView) {
								buf := GetState(s).BufferViews.Get(bufferView).Buffer.VulkanHandle
								// Advance the read/modify behavior of the descriptors from
								// draw and dispatch calls to here. Details in the handling
								// of vkCmdDispatch and vkCmdDraw.
								readMemoryBindingsData(b, getOverlappedBindingsForBuffer(buf))
							}
						})
					}
				}
			}
		}

	case *RecreateCmdBindDescriptorSets:
		descriptorSetCount := a.DescriptorSetCount
		descriptorSets := a.PDescriptorSets.Slice(0, uint64(descriptorSetCount), s)
		for i := uint32(0); i < descriptorSetCount; i++ {
			descriptorSet := descriptorSets.Index(uint64(i), s).Read(ctx, a, s, nil)
			addRead(&b, g, vulkanStateKey(descriptorSet))
			if GetState(s).DescriptorSets.Contains(descriptorSet) {
				for _, descBinding := range GetState(s).DescriptorSets.Get(descriptorSet).Bindings {
					for _, bufferInfo := range descBinding.BufferBinding {
						buf := bufferInfo.Buffer

						recordCommand(&b, a.CommandBuffer, func(b *AtomBehaviour) {
							// Descriptors might be modified
							addModify(b, g, vulkanStateKey(buf))
						})
					}
					for _, imageInfo := range descBinding.ImageBinding {
						view := imageInfo.ImageView

						recordCommand(&b, a.CommandBuffer, func(b *AtomBehaviour) {
							addRead(b, g, vulkanStateKey(view))
						})
					}
					for _, bufferView := range descBinding.BufferViewBindings {

						recordCommand(&b, a.CommandBuffer, func(b *AtomBehaviour) {
							addRead(b, g, vulkanStateKey(bufferView))
						})
					}
				}
			}
		}

	case *VkBeginCommandBuffer:
		cmdbuf := g.getOrCreateCommandBuffer(a.CommandBuffer)
		addRead(&b, g, cmdbuf.handle)
		addWrite(&b, g, cmdbuf.records)

	case *VkEndCommandBuffer:
		cmdbuf := g.getOrCreateCommandBuffer(a.CommandBuffer)
		addModify(&b, g, cmdbuf)

	case *RecreateAndBeginCommandBuffer:
		cmdbuf := g.getOrCreateCommandBuffer(a.PCommandBuffer.Read(ctx, a, s, nil))
		addWrite(&b, g, cmdbuf)

	case *RecreateEndCommandBuffer:
		cmdbuf := g.getOrCreateCommandBuffer(a.CommandBuffer)
		addModify(&b, g, cmdbuf)

	case *VkCmdPipelineBarrier:
		recordCommand(&b, a.CommandBuffer, func(b *AtomBehaviour) {})
		//TODO: handle the image and buffer memory barriers?

	case *RecreateCmdPipelineBarrier:
		recordCommand(&b, a.CommandBuffer, func(b *AtomBehaviour) {})
		//TODO: handle the image and buffer memory barriers?

	case *VkCmdBindPipeline:
		recordCommand(&b, a.CommandBuffer, func(b *AtomBehaviour) {
			addRead(b, g, vulkanStateKey(a.Pipeline))
		})
		addRead(&b, g, vulkanStateKey(a.Pipeline))

	case *RecreateCmdBindPipeline:
		recordCommand(&b, a.CommandBuffer, func(b *AtomBehaviour) {
			addRead(b, g, vulkanStateKey(a.Pipeline))
		})
		addRead(&b, g, vulkanStateKey(a.Pipeline))

	case *VkCmdBeginQuery:
		recordCommand(&b, a.CommandBuffer, func(b *AtomBehaviour) {})

	case *RecreateCmdBeginQuery:
		recordCommand(&b, a.CommandBuffer, func(b *AtomBehaviour) {})

	case *VkCmdEndQuery:
		recordCommand(&b, a.CommandBuffer, func(b *AtomBehaviour) {})

	case *RecreateCmdEndQuery:
		recordCommand(&b, a.CommandBuffer, func(b *AtomBehaviour) {})

	case *VkCmdResetQueryPool:
		recordCommand(&b, a.CommandBuffer, func(b *AtomBehaviour) {})

	case *RecreateCmdResetQueryPool:
		recordCommand(&b, a.CommandBuffer, func(b *AtomBehaviour) {})

	case *VkCmdClearAttachments:
		recordCommand(&b, a.CommandBuffer, func(b *AtomBehaviour) {})

	case *RecreateCmdClearAttachments:
		recordCommand(&b, a.CommandBuffer, func(b *AtomBehaviour) {})
		//TODO: handle the case that the attachment is fully cleared.

	case *VkCmdClearColorImage:
		recordCommand(&b, a.CommandBuffer, func(b *AtomBehaviour) {})
		//TODO: handle the color image

	case *RecreateCmdClearColorImage:
		recordCommand(&b, a.CommandBuffer, func(b *AtomBehaviour) {})
		//TODO: handle the color image

	case *VkCmdClearDepthStencilImage:
		recordCommand(&b, a.CommandBuffer, func(b *AtomBehaviour) {})
		//TODO: handle the depth/stencil image

	case *RecreateCmdClearDepthStencilImage:
		recordCommand(&b, a.CommandBuffer, func(b *AtomBehaviour) {})
		//TODO: handle the depth/stencil image

	case *VkCmdSetDepthBias:
		recordCommand(&b, a.CommandBuffer, func(b *AtomBehaviour) {})

	case *RecreateCmdSetDepthBias:
		recordCommand(&b, a.CommandBuffer, func(b *AtomBehaviour) {})

	case *VkCmdSetBlendConstants:
		recordCommand(&b, a.CommandBuffer, func(b *AtomBehaviour) {})

	case *RecreateCmdSetBlendConstants:
		recordCommand(&b, a.CommandBuffer, func(b *AtomBehaviour) {})

	case *VkCmdExecuteCommands:
		secondaryCmdBufs := a.PCommandBuffers.Slice(0, uint64(a.CommandBufferCount), s)
		for i := uint32(0); i < a.CommandBufferCount; i++ {
			secondaryCmdBuf := secondaryCmdBufs.Index(uint64(i), s).Read(ctx, a, s, nil)
			scb := g.getOrCreateCommandBuffer(secondaryCmdBuf)
			addRead(&b, g, scb)
			recordCommand(&b, a.CommandBuffer, func(b *AtomBehaviour) {
				for _, c := range scb.records.Commands {
					c(b)
				}
			})
		}

	case *RecreateCmdExecuteCommands:
		secondaryCmdBufs := a.PCommandBuffers.Slice(0, uint64(a.CommandBufferCount), s)
		for i := uint32(0); i < a.CommandBufferCount; i++ {
			secondaryCmdBuf := secondaryCmdBufs.Index(uint64(i), s).Read(ctx, a, s, nil)
			scb := g.getOrCreateCommandBuffer(secondaryCmdBuf)
			addRead(&b, g, scb)
			recordCommand(&b, a.CommandBuffer, func(b *AtomBehaviour) {
				for _, c := range scb.records.Commands {
					c(b)
				}
			})
		}

	case *VkQueueSubmit:
		// Queue submit atom should always be alive
		b.KeepAlive = true

		// handle queue
		addModify(&b, g, vulkanStateKey(a.Queue))

		// handle command buffers
		submitCount := a.SubmitCount
		submits := a.PSubmits.Slice(0, uint64(submitCount), s)
		for i := uint32(0); i < submitCount; i++ {
			submit := submits.Index(uint64(i), s).Read(ctx, a, s, nil)
			commandBufferCount := submit.CommandBufferCount
			commandBuffers := submit.PCommandBuffers.Slice(0, uint64(commandBufferCount), s)
			for j := uint32(0); j < submit.CommandBufferCount; j++ {
				vkCmdBuf := commandBuffers.Index(uint64(j), s).Read(ctx, a, s, nil)
				cb := g.getOrCreateCommandBuffer(vkCmdBuf)
				// All the commands that are submitted will not be dropped.
				addRead(&b, g, cb)

				// Carry out the behaviors in the recorded commands.
				for _, c := range cb.records.Commands {
					c(&b)
				}
			}
		}

	case *VkQueuePresentKHR:
		addRead(&b, g, vulkanStateKey(a.Queue))
		g.roots[g.addressMap.addressOf(vulkanStateKey(a.Queue))] = true
		b.KeepAlive = true

	default:
		// TODO: handle vkGetDeviceMemoryCommitment, VkSparseMemoryBind and other
		// commands
		b.KeepAlive = true
		debug("\tNot handled by DCE, kept alive")
	}
	return b
}

// Traverse through the given VkWriteDescriptorSet slice, add behaviors to
// |b| according to the descriptor type.
func processDescriptorWrites(writes VkWriteDescriptorSetˢ, b *AtomBehaviour, g *DependencyGraph, ctx context.Context, a atom.Atom, s *gfxapi.State) error {
	writeCount := writes.Info().Count
	for i := uint64(0); i < writeCount; i++ {
		write := writes.Index(uint64(i), s).Read(ctx, a, s, nil)
		if write.DescriptorCount > 0 {
			// handle the target descriptor set
			b.modify(g, vulkanStateKey(write.DstSet))
			switch write.DescriptorType {
			case VkDescriptorType_VK_DESCRIPTOR_TYPE_SAMPLER,
				VkDescriptorType_VK_DESCRIPTOR_TYPE_COMBINED_IMAGE_SAMPLER,
				VkDescriptorType_VK_DESCRIPTOR_TYPE_SAMPLED_IMAGE,
				VkDescriptorType_VK_DESCRIPTOR_TYPE_STORAGE_IMAGE,
				VkDescriptorType_VK_DESCRIPTOR_TYPE_INPUT_ATTACHMENT:
				imageInfos := write.PImageInfo.Slice(0, uint64(write.DescriptorCount), s)
				for j := uint64(0); j < imageInfos.Info().Count; j++ {
					imageInfo := imageInfos.Index(uint64(j), s).Read(ctx, a, s, nil)
					sampler := imageInfo.Sampler
					imageView := imageInfo.ImageView
					b.read(g, vulkanStateKey(sampler))
					b.read(g, vulkanStateKey(imageView))
				}
			case VkDescriptorType_VK_DESCRIPTOR_TYPE_UNIFORM_BUFFER,
				VkDescriptorType_VK_DESCRIPTOR_TYPE_STORAGE_BUFFER,
				VkDescriptorType_VK_DESCRIPTOR_TYPE_UNIFORM_BUFFER_DYNAMIC,
				VkDescriptorType_VK_DESCRIPTOR_TYPE_STORAGE_BUFFER_DYNAMIC:
				bufferInfos := write.PBufferInfo.Slice(0, uint64(write.DescriptorCount), s)
				for j := uint64(0); j < bufferInfos.Info().Count; j++ {
					bufferInfo := bufferInfos.Index(uint64(j), s).Read(ctx, a, s, nil)
					buffer := bufferInfo.Buffer
					b.read(g, vulkanStateKey(buffer))
				}
			case VkDescriptorType_VK_DESCRIPTOR_TYPE_UNIFORM_TEXEL_BUFFER,
				VkDescriptorType_VK_DESCRIPTOR_TYPE_STORAGE_TEXEL_BUFFER:
				bufferViews := write.PTexelBufferView.Slice(0, uint64(write.DescriptorCount), s)
				for j := uint64(0); j < bufferViews.Info().Count; j++ {
					bufferView := bufferViews.Index(uint64(j), s).Read(ctx, a, s, nil)
					b.read(g, vulkanStateKey(bufferView))
				}
			default:
				return fmt.Errorf("Unhandled DescriptorType: %v", write.DescriptorType)
			}
		}
	}
	return nil
}
