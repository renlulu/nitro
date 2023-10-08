// Copyright 2021-2023, Offchain Labs, Inc.
// For license information, see https://github.com/OffchainLabs/nitro/blob/master/LICENSE

mod stylus;

pub use stylus::*;

use arbutil::wavm;
use fnv::FnvHashSet as HashSet;
use go_abi::*;
use go_js::{JsEnv, JsState, JsValueId};
use rand::RngCore;
use rand_pcg::Pcg32;
use std::{collections::BinaryHeap, convert::TryFrom, io::Write};

unsafe fn read_value_ids(mut ptr: u64, len: u64) -> Vec<JsValueId> {
    let mut values = Vec::new();
    for _ in 0..len {
        let p = usize::try_from(ptr).expect("Go pointer didn't fit in usize");
        values.push(JsValueId(wavm::caller_load64(p)));
        ptr += 8;
    }
    values
}

#[no_mangle]
pub unsafe extern "C" fn go__debug(x: usize) {
    println!("go debug: {}", x);
}

#[no_mangle]
pub unsafe extern "C" fn go__runtime_resetMemoryDataView(_: usize) {}

/// Safety: λ(code int32)
#[no_mangle]
pub unsafe extern "C" fn go__runtime_wasmExit(sp: usize) {
    let mut sp = GoStack::new(sp);
    std::process::exit(sp.read_u32() as i32);
}

/// Safety: λ(fd uintptr, p pointer, len int32)
#[no_mangle]
pub unsafe extern "C" fn go__runtime_wasmWrite(sp: usize) {
    let mut sp = GoStack::new(sp);
    let fd = sp.read_u64();
    let ptr = sp.read_u64();
    let len = sp.read_u32();
    let buf = wavm::read_slice(ptr, len.into());
    if fd == 2 {
        let stderr = std::io::stderr();
        let mut stderr = stderr.lock();
        stderr.write_all(&buf).unwrap();
    } else {
        let stdout = std::io::stdout();
        let mut stdout = stdout.lock();
        stdout.write_all(&buf).unwrap();
    }
}

// An increasing clock used when Go asks for time, measured in nanoseconds.
static mut TIME: u64 = 0;
// The amount of TIME advanced each check. Currently 10 milliseconds.
static mut TIME_INTERVAL: u64 = 10_000_000;

/// Safety: λ() int64
#[no_mangle]
pub unsafe extern "C" fn go__runtime_nanotime1(sp: usize) {
    let mut sp = GoStack::new(sp);
    TIME += TIME_INTERVAL;
    sp.write_u64(TIME);
}

/// Safety: λ() (seconds int64, nanos int32)
#[no_mangle]
pub unsafe extern "C" fn go__runtime_walltime(sp: usize) {
    let mut sp = GoStack::new(sp);
    TIME += TIME_INTERVAL;
    sp.write_u64(TIME / 1_000_000_000);
    sp.write_u32((TIME % 1_000_000_000) as u32);
}

#[no_mangle]
pub unsafe extern "C" fn go__runtime_walltime1(sp: usize) {
    let mut sp = GoStack::new(sp);
    TIME += TIME_INTERVAL;
    sp.write_u64(TIME / 1_000_000_000);
    sp.write_u64(TIME % 1_000_000_000);
}

static mut RNG: Option<Pcg32> = None;

unsafe fn get_rng<'a>() -> &'a mut Pcg32 {
    RNG.get_or_insert_with(|| Pcg32::new(0xcafef00dd15ea5e5, 0xa02bdbf7bb3c0a7))
}

/// Safety: λ(dest []byte)
#[no_mangle]
pub unsafe extern "C" fn go__runtime_getRandomData(sp: usize) {
    let mut sp = GoStack::new(sp);
    let rng = get_rng();
    let mut ptr = usize::try_from(sp.read_u64()).expect("Go getRandomData pointer not a usize");
    let mut len = sp.read_u64();
    while len >= 4 {
        wavm::caller_store32(ptr, rng.next_u32());
        ptr += 4;
        len -= 4;
    }
    if len > 0 {
        let mut rem = rng.next_u32();
        for _ in 0..len {
            wavm::caller_store8(ptr, rem as u8);
            ptr += 1;
            rem >>= 8;
        }
    }
}

#[derive(Debug, Clone, PartialEq, Eq)]
struct TimeoutInfo {
    time: u64,
    id: u32,
}

impl Ord for TimeoutInfo {
    fn cmp(&self, other: &Self) -> std::cmp::Ordering {
        other
            .time
            .cmp(&self.time)
            .then_with(|| other.id.cmp(&self.id))
    }
}

impl PartialOrd for TimeoutInfo {
    fn partial_cmp(&self, other: &Self) -> Option<std::cmp::Ordering> {
        Some(self.cmp(&other))
    }
}

#[derive(Default, Debug)]
struct TimeoutState {
    /// Contains tuples of (time, id)
    times: BinaryHeap<TimeoutInfo>,
    pending_ids: HashSet<u32>,
    next_id: u32,
}

static mut TIMEOUT_STATE: Option<TimeoutState> = None;

/// Safety: λ() (delay int64) int32
#[no_mangle]
pub unsafe extern "C" fn go__runtime_scheduleTimeoutEvent(sp: usize) {
    let mut sp = GoStack::new(sp);
    let mut time = sp.read_u64();
    time = time.saturating_mul(1_000_000); // milliseconds to nanoseconds
    time = time.saturating_add(TIME); // add the current time to the delay

    let state = TIMEOUT_STATE.get_or_insert_with(Default::default);
    let id = state.next_id;
    state.next_id += 1;
    state.times.push(TimeoutInfo { time, id });
    state.pending_ids.insert(id);

    sp.write_u32(id);
}

/// Safety: λ(id int32)
#[no_mangle]
pub unsafe extern "C" fn go__runtime_clearTimeoutEvent(sp: usize) {
    let mut sp = GoStack::new(sp);
    let id = sp.read_u32();

    let state = TIMEOUT_STATE.get_or_insert_with(Default::default);
    if !state.pending_ids.remove(&id) {
        eprintln!("Go attempting to clear not pending timeout event {}", id);
    }
}

macro_rules! unimpl_js {
    ($($f:ident),* $(,)?) => {
        $(
            #[no_mangle]
            pub unsafe extern "C" fn $f(_: usize) {
                unimplemented!("Go JS interface {} not supported", stringify!($f));
            }
        )*
    }
}

unimpl_js!(
    go__syscall_js_valuePrepareString,
    go__syscall_js_valueLoadString,
    go__syscall_js_valueDelete,
    go__syscall_js_valueInvoke,
    go__syscall_js_valueInstanceOf,
);

static mut JS: Option<JsState> = None;

unsafe fn get_js<'a>() -> &'a JsState {
    if JS.is_none() {
        JS = Some(JsState::new());
    }
    JS.as_ref().unwrap()
}

/// Safety: λ(v value, field string) value
#[no_mangle]
pub unsafe extern "C" fn go__syscall_js_valueGet(sp: usize) {
    let mut sp = GoStack::new(sp);
    let source = JsValueId(sp.read_u64());
    let field = sp.read_string();

    let result = get_js().value_get(source, &field);

    sp.write_u64(result.0);
}

struct WasmJsEnv;

impl JsEnv for WasmJsEnv {
    fn get_rng(&mut self) -> &mut dyn rand::RngCore {
        unsafe { get_rng() }
    }

    fn resume(&mut self) -> eyre::Result<()> {
        unsafe {
            wavm_guest_call__resume();
        }
        Ok(())
    }
}

/// Safety: λ(v value, args []value) (value, bool)
#[no_mangle]
pub unsafe extern "C" fn go__syscall_js_valueNew(sp: usize) {
    let mut sp = GoStack::new(sp);
    let constructor = JsValueId(sp.read_u64());
    let (args_ptr, args_len) = sp.read_go_slice();
    let args = read_value_ids(args_ptr, args_len);

    let result = get_js().value_new(&mut WasmJsEnv, constructor, &args);
    sp.restore_stack();

    match result {
        Ok(result) => {
            sp.write_u64(result.0);
            sp.write_u8(1);
        }
        Err(err) => {
            eprintln!("Go constructor call failed with error {err:#}");
            sp.write_u64(go_js::get_null().0);
            sp.write_u8(0);
        }
    }
}

/// Safety: λ(dest value, src []byte) (int, bool)
#[no_mangle]
pub unsafe extern "C" fn go__syscall_js_copyBytesToJS(sp: usize) {
    let mut sp = GoStack::new(sp);
    let dest_val = JsValueId(sp.read_u64());
    let (src_ptr, src_len) = sp.read_go_slice();

    get_js().copy_bytes_to_js(dest_val, |buf| {
        if buf.len() as u64 != src_len {
            eprintln!(
                "Go copying bytes from Go source length {} to JS dest length {}",
                src_len,
                buf.len(),
            );
        }
        let len = std::cmp::min(src_len, buf.len() as u64) as usize;
        // Slightly inefficient as this allocates a new temporary buffer
        buf[..len].copy_from_slice(&wavm::read_slice(src_ptr, len as u64));
        sp.write_u64(go_js::get_number(len as f64).0);
        sp.write_u8(1);
    });
}

/// Safety: λ(dest []byte, src value) (int, bool)
#[no_mangle]
pub unsafe extern "C" fn go__syscall_js_copyBytesToGo(sp: usize) {
    let mut sp = GoStack::new(sp);
    let (dest_ptr, dest_len) = sp.read_go_slice();
    let src_val = JsValueId(sp.read_u64());

    get_js().copy_bytes_to_go(src_val, |buf| {
        if buf.len() as u64 != dest_len {
            eprintln!(
                "Go copying bytes from JS source length {} to Go dest length {}",
                buf.len(),
                dest_len,
            );
        }
        let len = std::cmp::min(buf.len() as u64, dest_len) as usize;
        wavm::write_slice(&buf[..len], dest_ptr);

        sp.write_u64(go_js::get_number(len as f64).0);
        sp.write_u8(1);
    });
}

/// Safety: λ(array value, i int, v value)
#[no_mangle]
pub unsafe extern "C" fn go__syscall_js_valueSetIndex(sp: usize) {
    let mut sp = GoStack::new(sp);
    let source = JsValueId(sp.read_u64());
    let index = sp.read_go_ptr() as usize;
    let value = JsValueId(sp.read_u64());

    get_js().value_set_index(source, index, value);
}

/// Safety: λ(v value, method string, args []value) (value, bool)
#[no_mangle]
pub unsafe extern "C" fn go__syscall_js_valueCall(sp: usize) {
    let mut sp = GoStack::new(sp);
    let object = JsValueId(sp.read_u64());
    let method_name = sp.read_string();
    let (args_ptr, args_len) = sp.read_go_slice();
    let args = read_value_ids(args_ptr, args_len);

    let result = get_js().value_call(&mut WasmJsEnv, object, &method_name, &args);
    sp.restore_stack();

    match result {
        Ok(result) => {
            sp.write_u64(result.0);
            sp.write_u8(1);
        }
        Err(err) => {
            eprintln!("Go method call to {method_name} failed with error {err:#}");
            sp.write_u64(go_js::get_null().0);
            sp.write_u8(0);
        }
    }
}

/// Safety: λ(v value, field string, x value)
#[no_mangle]
pub unsafe extern "C" fn go__syscall_js_valueSet(sp: usize) {
    let mut sp = GoStack::new(sp);
    let source = JsValueId(sp.read_u64());
    let field = sp.read_string();
    let new_value = JsValueId(sp.read_u64());

    get_js().value_set(source, &field, new_value);
}

/// Safety: λ(v string) value
#[no_mangle]
pub unsafe extern "C" fn go__syscall_js_stringVal(sp: usize) {
    let mut sp = GoStack::new(sp);
    let data = sp.read_string();
    let value = get_js().string_val(data);
    sp.write_u64(value.0);
}

/// Safety: λ(v value) int
#[no_mangle]
pub unsafe extern "C" fn go__syscall_js_valueLength(sp: usize) {
    let mut sp = GoStack::new(sp);

    let source = JsValueId(sp.read_u64());
    let length = get_js().value_length(source);

    sp.write_u64(length as u64);
}

/// Safety: λ(v value, i int) value
#[no_mangle]
pub unsafe extern "C" fn go__syscall_js_valueIndex(sp: usize) {
    let mut sp = GoStack::new(sp);
    let source = JsValueId(sp.read_u64());
    let index = sp.read_ptr::<*const u8>() as usize;

    let result = get_js().value_index(source, index);

    sp.write_u64(result.0);
}

/// Safety: λ(v value)
/// TODO: reference counting
#[no_mangle]
pub unsafe extern "C" fn go__syscall_js_finalizeRef(sp: usize) {
    let mut sp = GoStack::new(sp);
    let val = JsValueId(sp.read_u64());
    get_js().finalize_ref(val);
}

#[no_mangle]
pub unsafe extern "C" fn wavm__go_after_run() {
    let mut state = TIMEOUT_STATE.get_or_insert_with(Default::default);
    while let Some(info) = state.times.pop() {
        while state.pending_ids.contains(&info.id) {
            TIME = std::cmp::max(TIME, info.time);
            // Important: the current reference to state shouldn't be used after this resume call,
            // as it might during the resume call the reference might be invalidated.
            // That's why immediately after this resume call, we replace the reference
            // with a new reference to TIMEOUT_STATE.
            wavm_guest_call__resume();
            state = TIMEOUT_STATE.get_or_insert_with(Default::default);
        }
    }
}
