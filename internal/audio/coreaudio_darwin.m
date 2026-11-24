#import <Foundation/Foundation.h>
#import <CoreAudio/CoreAudio.h>
#import <CoreAudio/CATapDescription.h>
#import <CoreAudio/AudioHardwareTapping.h>
#import <AudioToolbox/AudioToolbox.h>
#include <stdatomic.h>
#include <string.h>
#include <stdio.h>
#include <math.h>
#include <time.h>
#include <unistd.h>
#include "coreaudio_darwin.h"

#define TARGET_SAMPLE_RATE 48000.0
#define CAP_RING_FRAMES 65536
#define CAP_RING_MASK (CAP_RING_FRAMES - 1)
#define MON_RING_FRAMES 262144
#define MON_RING_MASK (MON_RING_FRAMES - 1)

static void set_err(char *buf, int len, const char *msg, OSStatus st) {
	if (!buf || len <= 0) return;
	if (st != noErr) {
		snprintf(buf, (size_t)len, "%s (OSStatus=%d)", msg, (int)st);
	} else {
		snprintf(buf, (size_t)len, "%s", msg);
	}
}

int64_t ss_now_ns(void) {
	return (int64_t)clock_gettime_nsec_np(CLOCK_UPTIME_RAW);
}

static void cfstring_to_cstr(CFStringRef s, char *out, int max_len) {
	if (!out || max_len <= 0) return;
	out[0] = '\0';
	if (!s) return;
	CFStringGetCString(s, out, (CFIndex)max_len, kCFStringEncodingUTF8);
}

static int get_device_channels(AudioDeviceID dev, AudioObjectPropertyScope scope) {
	AudioObjectPropertyAddress addr = {
		kAudioDevicePropertyStreamConfiguration,
		scope,
		kAudioObjectPropertyElementMain
	};
	UInt32 size = 0;
	if (AudioObjectGetPropertyDataSize(dev, &addr, 0, NULL, &size) != noErr || size == 0) {
		return 0;
	}
	AudioBufferList *abl = (AudioBufferList *)malloc(size);
	if (!abl) return 0;
	int channels = 0;
	if (AudioObjectGetPropertyData(dev, &addr, 0, NULL, &size, abl) == noErr) {
		for (UInt32 i = 0; i < abl->mNumberBuffers; i++) {
			channels += (int)abl->mBuffers[i].mNumberChannels;
		}
	}
	free(abl);
	return channels;
}

static AudioDeviceID get_default_device(AudioObjectPropertySelector sel) {
	AudioDeviceID dev = kAudioObjectUnknown;
	UInt32 size = sizeof(dev);
	AudioObjectPropertyAddress addr = {
		sel,
		kAudioObjectPropertyScopeGlobal,
		kAudioObjectPropertyElementMain
	};
	if (AudioObjectGetPropertyData(kAudioObjectSystemObject, &addr, 0, NULL, &size, &dev) != noErr) {
		return kAudioObjectUnknown;
	}
	return dev;
}

int ss_list_devices(SSDeviceInfo *out, int max_count) {
	if (!out || max_count <= 0) return 0;

	AudioObjectPropertyAddress addr = {
		kAudioHardwarePropertyDevices,
		kAudioObjectPropertyScopeGlobal,
		kAudioObjectPropertyElementMain
	};
	UInt32 size = 0;
	if (AudioObjectGetPropertyDataSize(kAudioObjectSystemObject, &addr, 0, NULL, &size) != noErr || size == 0) {
		return 0;
	}
	int count = (int)(size / sizeof(AudioDeviceID));
	AudioDeviceID *devs = (AudioDeviceID *)malloc(size);
	if (!devs) return 0;

	if (AudioObjectGetPropertyData(kAudioObjectSystemObject, &addr, 0, NULL, &size, devs) != noErr) {
		free(devs);
		return 0;
	}

	AudioDeviceID def_in = get_default_device(kAudioHardwarePropertyDefaultInputDevice);
	AudioDeviceID def_out = get_default_device(kAudioHardwarePropertyDefaultOutputDevice);

	int written = 0;
	for (int i = 0; i < count && written < max_count; i++) {
		AudioDeviceID d = devs[i];
		char uid[128] = {0};
		char name[128] = {0};

		CFStringRef cf_uid = NULL;
		UInt32 usize = sizeof(cf_uid);
		AudioObjectPropertyAddress uaddr = {
			kAudioDevicePropertyDeviceUID,
			kAudioObjectPropertyScopeGlobal,
			kAudioObjectPropertyElementMain
		};
		if (AudioObjectGetPropertyData(d, &uaddr, 0, NULL, &usize, &cf_uid) == noErr && cf_uid) {
			cfstring_to_cstr(cf_uid, uid, sizeof(uid));
			CFRelease(cf_uid);
		}
		if (strncmp(uid, "com.roomchorus.tap.agg.", 23) == 0) {
			continue;
		}

		CFStringRef cf_name = NULL;
		UInt32 nsize = sizeof(cf_name);
		AudioObjectPropertyAddress naddr = {
			kAudioObjectPropertyName,
			kAudioObjectPropertyScopeGlobal,
			kAudioObjectPropertyElementMain
		};
		if (AudioObjectGetPropertyData(d, &naddr, 0, NULL, &nsize, &cf_name) == noErr && cf_name) {
			cfstring_to_cstr(cf_name, name, sizeof(name));
			CFRelease(cf_name);
		}

		int has_in = get_device_channels(d, kAudioObjectPropertyScopeInput) > 0 ? 1 : 0;
		int has_out = get_device_channels(d, kAudioObjectPropertyScopeOutput) > 0 ? 1 : 0;
		if (!has_in && !has_out) continue;

		Float64 sr = 48000.0;
		UInt32 sr_size = sizeof(sr);
		AudioObjectPropertyAddress sr_addr = {
			kAudioDevicePropertyNominalSampleRate,
			kAudioObjectPropertyScopeGlobal,
			kAudioObjectPropertyElementMain
		};
		AudioObjectGetPropertyData(d, &sr_addr, 0, NULL, &sr_size, &sr);

		SSDeviceInfo *info = &out[written++];
		memset(info, 0, sizeof(*info));
		info->id = (uint32_t)d;
		snprintf(info->uid, sizeof(info->uid), "%s", uid);
		snprintf(info->name, sizeof(info->name), "%s", name[0] ? name : uid);
		info->sample_rate = (double)sr;
		info->has_input = has_in;
		info->has_output = has_out;
		info->is_default_input = (d == def_in) ? 1 : 0;
		info->is_default_output = (d == def_out) ? 1 : 0;
	}

	free(devs);
	return written;
}

// --- Capture state ---
static float g_cap_ring[CAP_RING_FRAMES * 2];
static atomic_uint_fast64_t g_cap_write_pos = 0;
static atomic_uint_fast64_t g_cap_read_pos = 0;
static atomic_int_fast64_t g_cap_last_write_ns = 0;

static AudioObjectID g_tap_id = kAudioObjectUnknown;
static AudioDeviceID g_cap_device_id = kAudioObjectUnknown;
static AudioDeviceIOProcID g_cap_ioproc_id = NULL;
static int g_cap_is_aggregate = 0;
static AudioUnit g_keepalive_unit = NULL;

static AudioStreamBasicDescription g_cap_asbd;
static double g_resamp_phase = 0.0;
static float g_resamp_prev_l = 0.0f;
static float g_resamp_prev_r = 0.0f;
static int g_resamp_init = 0;

static void cap_ring_reset(void) {
	atomic_store_explicit(&g_cap_write_pos, 0, memory_order_relaxed);
	atomic_store_explicit(&g_cap_read_pos, 0, memory_order_relaxed);
	atomic_store_explicit(&g_cap_last_write_ns, ss_now_ns(), memory_order_relaxed);
	g_resamp_phase = 0.0;
	g_resamp_prev_l = 0.0f;
	g_resamp_prev_r = 0.0f;
	g_resamp_init = 0;
}

static inline void cap_ring_push_frame(float l, float r) {
	uint64_t w = atomic_load_explicit(&g_cap_write_pos, memory_order_relaxed);
	uint64_t rd = atomic_load_explicit(&g_cap_read_pos, memory_order_acquire);
	if (w - rd >= CAP_RING_FRAMES - 1) {
		// Drop oldest frame on consumer stall to keep latency bounded.
		atomic_store_explicit(&g_cap_read_pos, w - (CAP_RING_FRAMES - 2), memory_order_release);
	}
	uint64_t idx = (w & CAP_RING_MASK) * 2;
	g_cap_ring[idx] = l;
	g_cap_ring[idx + 1] = r;
	atomic_store_explicit(&g_cap_write_pos, w + 1, memory_order_release);
}

static inline void read_sample_pair(const AudioBufferList *abl, UInt32 frame, UInt32 totalFrames, float *outL, float *outR) {
	if (!abl || abl->mNumberBuffers == 0 || frame >= totalFrames) {
		*outL = 0.0f;
		*outR = 0.0f;
		return;
	}

	const AudioStreamBasicDescription *fmt = &g_cap_asbd;
	int is_float = (fmt->mFormatFlags & kAudioFormatFlagIsFloat) != 0;
	int is_int = (fmt->mFormatFlags & kAudioFormatFlagIsSignedInteger) != 0;
	int non_interleaved = (abl->mNumberBuffers >= 2 && abl->mBuffers[0].mNumberChannels == 1);

	if (non_interleaved) {
		const AudioBuffer *b0 = &abl->mBuffers[0];
		const AudioBuffer *b1 = (abl->mNumberBuffers > 1 && abl->mBuffers[1].mNumberChannels == 1) ? &abl->mBuffers[1] : b0;
		if (is_float && fmt->mBitsPerChannel == 32 && b0->mData && b1->mData) {
			*outL = ((const float *)b0->mData)[frame];
			*outR = ((const float *)b1->mData)[frame];
			return;
		}
		if (is_int && fmt->mBitsPerChannel == 16 && b0->mData && b1->mData) {
			*outL = (float)((const int16_t *)b0->mData)[frame] / 32768.0f;
			*outR = (float)((const int16_t *)b1->mData)[frame] / 32768.0f;
			return;
		}
		if (is_int && fmt->mBitsPerChannel == 32 && b0->mData && b1->mData) {
			*outL = (float)((const int32_t *)b0->mData)[frame] / 2147483648.0f;
			*outR = (float)((const int32_t *)b1->mData)[frame] / 2147483648.0f;
			return;
		}
	}

	const AudioBuffer *b = &abl->mBuffers[0];
	UInt32 ch = b->mNumberChannels;
	if (!b->mData || ch == 0) {
		*outL = 0.0f;
		*outR = 0.0f;
		return;
	}
	if (is_float || (!is_int && fmt->mBitsPerChannel == 32)) {
		const float *p = (const float *)b->mData;
		if (ch == 1) {
			float v = p[frame];
			*outL = v;
			*outR = v;
		} else {
			*outL = p[frame * ch];
			*outR = p[frame * ch + 1];
		}
		return;
	}
	if (is_int && fmt->mBitsPerChannel == 16) {
		const int16_t *p = (const int16_t *)b->mData;
		if (ch == 1) {
			float v = (float)p[frame] / 32768.0f;
			*outL = v;
			*outR = v;
		} else {
			*outL = (float)p[frame * ch] / 32768.0f;
			*outR = (float)p[frame * ch + 1] / 32768.0f;
		}
		return;
	}
	if (is_int && fmt->mBitsPerChannel == 32) {
		const int32_t *p = (const int32_t *)b->mData;
		if (ch == 1) {
			float v = (float)p[frame] / 2147483648.0f;
			*outL = v;
			*outR = v;
		} else {
			*outL = (float)p[frame * ch] / 2147483648.0f;
			*outR = (float)p[frame * ch + 1] / 2147483648.0f;
		}
		return;
	}

	*outL = 0.0f;
	*outR = 0.0f;
}

static OSStatus capture_ioproc(AudioObjectID inDevice,
							   const AudioTimeStamp *inNow,
							   const AudioBufferList *inInputData,
							   const AudioTimeStamp *inInputTime,
							   AudioBufferList *outOutputData,
							   const AudioTimeStamp *inOutputTime,
							   void *inClientData) {
	(void)inDevice;
	(void)inNow;
	(void)inInputTime;
	(void)inOutputTime;
	(void)inClientData;

	if (outOutputData) {
		for (UInt32 i = 0; i < outOutputData->mNumberBuffers; i++) {
			if (outOutputData->mBuffers[i].mData && outOutputData->mBuffers[i].mDataByteSize > 0) {
				memset(outOutputData->mBuffers[i].mData, 0, outOutputData->mBuffers[i].mDataByteSize);
			}
		}
	}

	if (!inInputData || inInputData->mNumberBuffers == 0) {
		return noErr;
	}

	const AudioBuffer *b0 = &inInputData->mBuffers[0];
	UInt32 bytesPerSample = g_cap_asbd.mBitsPerChannel > 0 ? (g_cap_asbd.mBitsPerChannel / 8) : sizeof(float);
	UInt32 channelsInBuf = b0->mNumberChannels > 0 ? b0->mNumberChannels : 1;
	UInt32 frameStride = bytesPerSample * channelsInBuf;
	if (frameStride == 0) frameStride = sizeof(float) * 2;
	UInt32 inFrames = b0->mDataByteSize / frameStride;
	if (inFrames == 0) return noErr;

	double inRate = g_cap_asbd.mSampleRate > 1000.0 ? g_cap_asbd.mSampleRate : TARGET_SAMPLE_RATE;
	if (fabs(inRate - TARGET_SAMPLE_RATE) < 1.0) {
		for (UInt32 f = 0; f < inFrames; f++) {
			float l = 0.0f, r = 0.0f;
			read_sample_pair(inInputData, f, inFrames, &l, &r);
			cap_ring_push_frame(l, r);
		}
	} else {
		double step = inRate / TARGET_SAMPLE_RATE;
		for (UInt32 f = 0; f < inFrames; f++) {
			float curL = 0.0f, curR = 0.0f;
			read_sample_pair(inInputData, f, inFrames, &curL, &curR);
			if (!g_resamp_init) {
				g_resamp_prev_l = curL;
				g_resamp_prev_r = curR;
				g_resamp_init = 1;
			}
			while (g_resamp_phase < 1.0) {
				float t = (float)g_resamp_phase;
				float outL = g_resamp_prev_l + (curL - g_resamp_prev_l) * t;
				float outR = g_resamp_prev_r + (curR - g_resamp_prev_r) * t;
				cap_ring_push_frame(outL, outR);
				g_resamp_phase += step;
			}
			g_resamp_phase -= 1.0;
			g_resamp_prev_l = curL;
			g_resamp_prev_r = curR;
		}
	}

	atomic_store_explicit(&g_cap_last_write_ns, ss_now_ns(), memory_order_release);
	return noErr;
}

static AudioObjectID query_self_process_object_id(void) {
	pid_t pid = getpid();
	AudioObjectID procObj = kAudioObjectUnknown;
	UInt32 size = sizeof(procObj);
	AudioObjectPropertyAddress addr = {
		kAudioHardwarePropertyTranslatePIDToProcessObject,
		kAudioObjectPropertyScopeGlobal,
		kAudioObjectPropertyElementMain
	};
	if (AudioObjectGetPropertyData(kAudioObjectSystemObject, &addr, sizeof(pid), &pid, &size, &procObj) == noErr &&
		procObj != kAudioObjectUnknown) {
		return procObj;
	}

	// Fallback: iterate kAudioHardwarePropertyProcessObjectList
	AudioObjectPropertyAddress listAddr = {
		kAudioHardwarePropertyProcessObjectList,
		kAudioObjectPropertyScopeGlobal,
		kAudioObjectPropertyElementMain
	};
	UInt32 listSize = 0;
	if (AudioObjectGetPropertyDataSize(kAudioObjectSystemObject, &listAddr, 0, NULL, &listSize) != noErr || listSize == 0) {
		return kAudioObjectUnknown;
	}
	int n = (int)(listSize / sizeof(AudioObjectID));
	AudioObjectID *objs = (AudioObjectID *)malloc(listSize);
	if (!objs) return kAudioObjectUnknown;

	AudioObjectID found = kAudioObjectUnknown;
	if (AudioObjectGetPropertyData(kAudioObjectSystemObject, &listAddr, 0, NULL, &listSize, objs) == noErr) {
		for (int i = 0; i < n; i++) {
			pid_t objPid = 0;
			UInt32 psize = sizeof(objPid);
			AudioObjectPropertyAddress paddr = {
				kAudioProcessPropertyPID,
				kAudioObjectPropertyScopeGlobal,
				kAudioObjectPropertyElementMain
			};
			if (AudioObjectGetPropertyData(objs[i], &paddr, 0, NULL, &psize, &objPid) == noErr && objPid == pid) {
				found = objs[i];
				break;
			}
		}
	}
	free(objs);
	return found;
}

static void ensure_hal_client_registered(void) {
	if (query_self_process_object_id() != kAudioObjectUnknown) {
		return;
	}
	if (!g_keepalive_unit) {
		AudioComponentDescription desc = {0};
		desc.componentType = kAudioUnitType_Output;
		desc.componentSubType = kAudioUnitSubType_HALOutput;
		desc.componentManufacturer = kAudioUnitManufacturer_Apple;
		AudioComponent comp = AudioComponentFindNext(NULL, &desc);
		if (comp) {
			if (AudioComponentInstanceNew(comp, &g_keepalive_unit) == noErr && g_keepalive_unit) {
				AudioUnitInitialize(g_keepalive_unit);
			}
		}
	}
}

uint32_t ss_get_self_process_object_id(void) {
	ensure_hal_client_registered();
	return (uint32_t)query_self_process_object_id();
}

static void inspect_device_input_asbd(AudioDeviceID dev, AudioStreamBasicDescription *outAsbd) {
	memset(outAsbd, 0, sizeof(*outAsbd));
	outAsbd->mSampleRate = TARGET_SAMPLE_RATE;
	outAsbd->mFormatID = kAudioFormatLinearPCM;
	outAsbd->mFormatFlags = kAudioFormatFlagIsFloat | kAudioFormatFlagIsPacked;
	outAsbd->mChannelsPerFrame = 2;
	outAsbd->mBitsPerChannel = 32;
	outAsbd->mFramesPerPacket = 1;
	outAsbd->mBytesPerFrame = 8;
	outAsbd->mBytesPerPacket = 8;

	AudioObjectPropertyAddress streamsAddr = {
		kAudioDevicePropertyStreams,
		kAudioObjectPropertyScopeInput,
		kAudioObjectPropertyElementMain
	};
	UInt32 sz = 0;
	if (AudioObjectGetPropertyDataSize(dev, &streamsAddr, 0, NULL, &sz) == noErr && sz >= sizeof(AudioStreamID)) {
		AudioStreamID *streams = (AudioStreamID *)malloc(sz);
		if (streams) {
			if (AudioObjectGetPropertyData(dev, &streamsAddr, 0, NULL, &sz, streams) == noErr && sz >= sizeof(AudioStreamID)) {
				AudioStreamBasicDescription sfmt = {0};
				UInt32 fsz = sizeof(sfmt);
				AudioObjectPropertyAddress fmtAddr = {
					kAudioStreamPropertyVirtualFormat,
					kAudioObjectPropertyScopeGlobal,
					kAudioObjectPropertyElementMain
				};
				if (AudioObjectGetPropertyData(streams[0], &fmtAddr, 0, NULL, &fsz, &sfmt) == noErr && sfmt.mSampleRate > 0) {
					*outAsbd = sfmt;
					free(streams);
					return;
				}
			}
			free(streams);
		}
	}

	Float64 sr = 0;
	UInt32 srSize = sizeof(sr);
	AudioObjectPropertyAddress srAddr = {
		kAudioDevicePropertyNominalSampleRate,
		kAudioObjectPropertyScopeGlobal,
		kAudioObjectPropertyElementMain
	};
	if (AudioObjectGetPropertyData(dev, &srAddr, 0, NULL, &srSize, &sr) == noErr && sr > 1000.0) {
		outAsbd->mSampleRate = sr;
	}
}

void ss_stop_capture(void) {
	if (g_cap_device_id != kAudioObjectUnknown && g_cap_ioproc_id != NULL) {
		AudioDeviceStop(g_cap_device_id, g_cap_ioproc_id);
		AudioDeviceDestroyIOProcID(g_cap_device_id, g_cap_ioproc_id);
		g_cap_ioproc_id = NULL;
	}
	if (g_cap_is_aggregate && g_cap_device_id != kAudioObjectUnknown) {
		AudioHardwareDestroyAggregateDevice(g_cap_device_id);
	}
	g_cap_device_id = kAudioObjectUnknown;
	g_cap_is_aggregate = 0;

	if (@available(macOS 14.2, *)) {
		if (g_tap_id != kAudioObjectUnknown) {
			AudioHardwareDestroyProcessTap(g_tap_id);
			g_tap_id = kAudioObjectUnknown;
		}
	}

	if (g_keepalive_unit) {
		AudioUnitUninitialize(g_keepalive_unit);
		AudioComponentInstanceDispose(g_keepalive_unit);
		g_keepalive_unit = NULL;
	}
}

int ss_start_system_tap(int mute_original, uint32_t clock_device_id, char *err_buf, int err_buf_len) {
	ss_stop_capture();
	cap_ring_reset();

	if (@available(macOS 14.2, *)) {
		@autoreleasepool {
			ensure_hal_client_registered();
			AudioObjectID selfObj = query_self_process_object_id();
			NSArray<NSNumber *> *excludeList = (selfObj != kAudioObjectUnknown)
				? @[ @(selfObj) ]
				: @[];

			CATapDescription *tapDesc = [[CATapDescription alloc] initStereoGlobalTapButExcludeProcesses:excludeList];
			if (!tapDesc) {
				set_err(err_buf, err_buf_len, "failed to allocate CATapDescription", noErr);
				return -1;
			}
			tapDesc.name = @"RoomChorus System Tap";
			tapDesc.privateTap = YES;
			tapDesc.muteBehavior = mute_original ? CATapMutedWhenTapped : CATapUnmuted;

			AudioObjectID tapID = kAudioObjectUnknown;
			OSStatus st = AudioHardwareCreateProcessTap(tapDesc, &tapID);
			[tapDesc release];
			if (st != noErr || tapID == kAudioObjectUnknown) {
				set_err(err_buf, err_buf_len, "AudioHardwareCreateProcessTap failed", st);
				return -1;
			}
			g_tap_id = tapID;

			CFStringRef cfTapUID = NULL;
			UInt32 uidSize = sizeof(cfTapUID);
			AudioObjectPropertyAddress uidAddr = {
				kAudioTapPropertyUID,
				kAudioObjectPropertyScopeGlobal,
				kAudioObjectPropertyElementMain
			};
			st = AudioObjectGetPropertyData(tapID, &uidAddr, 0, NULL, &uidSize, &cfTapUID);
			if (st != noErr || !cfTapUID) {
				ss_stop_capture();
				set_err(err_buf, err_buf_len, "failed to read kAudioTapPropertyUID", st);
				return -1;
			}
			NSString *tapUID = [(NSString *)cfTapUID autorelease];

			AudioStreamBasicDescription tapAsbd = {0};
			UInt32 fmtSize = sizeof(tapAsbd);
			AudioObjectPropertyAddress fmtAddr = {
				kAudioTapPropertyFormat,
				kAudioObjectPropertyScopeGlobal,
				kAudioObjectPropertyElementMain
			};
			OSStatus tapFmtSt = AudioObjectGetPropertyData(tapID, &fmtAddr, 0, NULL, &fmtSize, &tapAsbd);

			// Determine optional clock UID from specified or default output device.
			NSString *clockUID = nil;
			AudioDeviceID clkDev = (clock_device_id != 0) ? (AudioDeviceID)clock_device_id : get_default_device(kAudioHardwarePropertyDefaultOutputDevice);
			if (clkDev != kAudioObjectUnknown) {
				CFStringRef cfClkUID = NULL;
				UInt32 csz = sizeof(cfClkUID);
				AudioObjectPropertyAddress caddr = {
					kAudioDevicePropertyDeviceUID,
					kAudioObjectPropertyScopeGlobal,
					kAudioObjectPropertyElementMain
				};
				if (AudioObjectGetPropertyData(clkDev, &caddr, 0, NULL, &csz, &cfClkUID) == noErr && cfClkUID) {
					clockUID = [(NSString *)cfClkUID autorelease];
				}
			}

			NSString *aggUID = [NSString stringWithFormat:@"com.roomchorus.tap.agg.%@", [[NSUUID UUID] UUIDString]];
			NSMutableDictionary *aggDesc = [NSMutableDictionary dictionary];
			aggDesc[@kAudioAggregateDeviceNameKey] = @"RoomChorus Tap Aggregate";
			aggDesc[@kAudioAggregateDeviceUIDKey] = aggUID;
			aggDesc[@kAudioAggregateDeviceIsPrivateKey] = @YES;
			aggDesc[@kAudioAggregateDeviceIsStackedKey] = @NO;
			aggDesc[@kAudioAggregateDeviceTapAutoStartKey] = @NO;
			aggDesc[@kAudioAggregateDeviceTapListKey] = @[
				@{
					@kAudioSubTapUIDKey : tapUID,
					@kAudioSubTapDriftCompensationKey : @YES
				}
			];
			if (clockUID) {
				aggDesc[@kAudioAggregateDeviceClockDeviceKey] = clockUID;
			}

			AudioDeviceID aggID = kAudioObjectUnknown;
			st = AudioHardwareCreateAggregateDevice((CFDictionaryRef)aggDesc, &aggID);
			if ((st != noErr || aggID == kAudioObjectUnknown) && clockUID) {
				// Fallback if macOS requires explicit main subdevice alongside clock.
				aggDesc[@kAudioAggregateDeviceMainSubDeviceKey] = clockUID;
				aggDesc[@kAudioAggregateDeviceSubDeviceListKey] = @[
					@{ @kAudioSubDeviceUIDKey : clockUID }
				];
				st = AudioHardwareCreateAggregateDevice((CFDictionaryRef)aggDesc, &aggID);
			}
			if (st != noErr || aggID == kAudioObjectUnknown) {
				ss_stop_capture();
				set_err(err_buf, err_buf_len, "AudioHardwareCreateAggregateDevice failed", st);
				return -1;
			}

			g_cap_device_id = aggID;
			g_cap_is_aggregate = 1;

			inspect_device_input_asbd(aggID, &g_cap_asbd);
			if (tapFmtSt == noErr && tapAsbd.mSampleRate > 1000.0 && tapAsbd.mChannelsPerFrame > 0) {
				g_cap_asbd = tapAsbd;
			}

			st = AudioDeviceCreateIOProcID(aggID, capture_ioproc, NULL, &g_cap_ioproc_id);
			if (st != noErr || !g_cap_ioproc_id) {
				ss_stop_capture();
				set_err(err_buf, err_buf_len, "AudioDeviceCreateIOProcID failed on tap aggregate", st);
				return -1;
			}
			st = AudioDeviceStart(aggID, g_cap_ioproc_id);
			if (st != noErr) {
				ss_stop_capture();
				set_err(err_buf, err_buf_len, "AudioDeviceStart failed on tap aggregate", st);
				return -1;
			}
			return 0;
		}
	} else {
		set_err(err_buf, err_buf_len, "CoreAudio Process Tap requires macOS 14.2 or newer", noErr);
		return -1;
	}
}

int ss_start_input_device(uint32_t device_id, char *err_buf, int err_buf_len) {
	ss_stop_capture();
	cap_ring_reset();

	AudioDeviceID dev = (device_id != 0) ? (AudioDeviceID)device_id : get_default_device(kAudioHardwarePropertyDefaultInputDevice);
	if (dev == kAudioObjectUnknown) {
		set_err(err_buf, err_buf_len, "no input audio device found", noErr);
		return -1;
	}
	if (get_device_channels(dev, kAudioObjectPropertyScopeInput) <= 0) {
		set_err(err_buf, err_buf_len, "selected device has no input channels", noErr);
		return -1;
	}

	g_cap_device_id = dev;
	g_cap_is_aggregate = 0;
	inspect_device_input_asbd(dev, &g_cap_asbd);

	OSStatus st = AudioDeviceCreateIOProcID(dev, capture_ioproc, NULL, &g_cap_ioproc_id);
	if (st != noErr || !g_cap_ioproc_id) {
		ss_stop_capture();
		set_err(err_buf, err_buf_len, "AudioDeviceCreateIOProcID failed on input device", st);
		return -1;
	}
	st = AudioDeviceStart(dev, g_cap_ioproc_id);
	if (st != noErr) {
		ss_stop_capture();
		set_err(err_buf, err_buf_len, "AudioDeviceStart failed on input device", st);
		return -1;
	}
	return 0;
}

int ss_capture_read(float *dst_interleaved, int frames_wanted, int64_t *out_capture_ns) {
	if (!dst_interleaved || frames_wanted <= 0) return 0;

	uint64_t w = atomic_load_explicit(&g_cap_write_pos, memory_order_acquire);
	uint64_t rd = atomic_load_explicit(&g_cap_read_pos, memory_order_acquire);
	if (w - rd < (uint64_t)frames_wanted) {
		return 0;
	}

	for (int i = 0; i < frames_wanted; i++) {
		uint64_t idx = ((rd + (uint64_t)i) & CAP_RING_MASK) * 2;
		dst_interleaved[2 * i] = g_cap_ring[idx];
		dst_interleaved[2 * i + 1] = g_cap_ring[idx + 1];
	}
	atomic_store_explicit(&g_cap_read_pos, rd + (uint64_t)frames_wanted, memory_order_release);

	if (out_capture_ns) {
		int64_t last_ns = atomic_load_explicit(&g_cap_last_write_ns, memory_order_acquire);
		int64_t frames_behind = (int64_t)(w - rd);
		int64_t delta_ns = (frames_behind * 1000000000LL) / (int64_t)TARGET_SAMPLE_RATE;
		*out_capture_ns = last_ns - delta_ns;
	}
	return frames_wanted;
}

// --- HostMonitor state ---
static float g_mon_ring[MON_RING_FRAMES * 2];
static atomic_uint_fast64_t g_mon_write_pos = 0;
static atomic_uint_fast64_t g_mon_anchor_frame = 0;
static atomic_int_fast64_t g_mon_anchor_ns = 0;

static _Atomic float g_mon_target_delay_ms = 300.0f;
static _Atomic float g_mon_gain = 1.0f;
static atomic_int g_mon_muted = 0;
static atomic_uint_fast64_t g_mon_underruns = 0;
static _Atomic double g_mon_actual_delay_ms = 0.0;

static AudioUnit g_mon_unit = NULL;
static double g_mon_read_pos = 0.0;
static int g_mon_initialized = 0;
static int g_mon_fade_in_rem = 0;

static void mon_ring_reset(void) {
	atomic_store_explicit(&g_mon_write_pos, 0, memory_order_relaxed);
	atomic_store_explicit(&g_mon_anchor_frame, 0, memory_order_relaxed);
	atomic_store_explicit(&g_mon_anchor_ns, ss_now_ns(), memory_order_relaxed);
	atomic_store_explicit(&g_mon_underruns, 0, memory_order_relaxed);
	atomic_store_explicit(&g_mon_actual_delay_ms, 0.0, memory_order_relaxed);
	g_mon_read_pos = 0.0;
	g_mon_initialized = 0;
	g_mon_fade_in_rem = 0;
}

static OSStatus monitor_render_cb(void *inRefCon,
								  AudioUnitRenderActionFlags *ioActionFlags,
								  const AudioTimeStamp *inTimeStamp,
								  UInt32 inBusNumber,
								  UInt32 inNumberFrames,
								  AudioBufferList *ioData) {
	(void)inRefCon;
	(void)ioActionFlags;
	(void)inTimeStamp;
	(void)inBusNumber;

	if (!ioData || ioData->mNumberBuffers == 0 || !ioData->mBuffers[0].mData) {
		return noErr;
	}
	float *out = (float *)ioData->mBuffers[0].mData;
	memset(out, 0, sizeof(float) * 2 * inNumberFrames);

	uint64_t writePos = atomic_load_explicit(&g_mon_write_pos, memory_order_acquire);
	uint64_t anchorFrame = atomic_load_explicit(&g_mon_anchor_frame, memory_order_acquire);
	int64_t anchorNs = atomic_load_explicit(&g_mon_anchor_ns, memory_order_acquire);
	float delayMs = atomic_load_explicit(&g_mon_target_delay_ms, memory_order_relaxed);
	float gain = atomic_load_explicit(&g_mon_gain, memory_order_relaxed);
	int muted = atomic_load_explicit(&g_mon_muted, memory_order_relaxed);

	if (writePos < (uint64_t)(inNumberFrames + 960)) {
		return noErr;
	}

	int64_t nowNs = ss_now_ns();
	int64_t targetPlayNs = nowNs - (int64_t)(delayMs * 1000000.0f);
	double desiredRead = (double)anchorFrame + (double)(targetPlayNs - anchorNs) * (TARGET_SAMPLE_RATE / 1000000000.0);
	double minAvail = (writePos > (uint64_t)(MON_RING_FRAMES - 2048)) ? (double)(writePos - (MON_RING_FRAMES - 2048)) : 0.0;
	double maxAvail = (double)writePos - (double)inNumberFrames - 4.0;

	if (desiredRead < minAvail || desiredRead > maxAvail) {
		g_mon_initialized = 0;
		atomic_store_explicit(&g_mon_actual_delay_ms, 0.0, memory_order_relaxed);
		return noErr;
	}

	if (!g_mon_initialized) {
		g_mon_read_pos = desiredRead;
		g_mon_initialized = 1;
		g_mon_fade_in_rem = 240;
	}

	double errFrames = desiredRead - g_mon_read_pos;
	if (fabs(errFrames) > 3600.0) {
		g_mon_read_pos = desiredRead;
		g_mon_fade_in_rem = 240;
		errFrames = 0.0;
	}

	double corr = errFrames * 0.00025;
	if (corr > 0.012) corr = 0.012;
	if (corr < -0.012) corr = -0.012;
	double step = 1.0 + corr;

	double actualDelay = ((double)anchorFrame - g_mon_read_pos) * (1000.0 / TARGET_SAMPLE_RATE) +
						 (double)(nowNs - anchorNs) / 1000000.0;
	atomic_store_explicit(&g_mon_actual_delay_ms, actualDelay, memory_order_relaxed);

	if (muted || gain <= 0.0001f) {
		g_mon_read_pos += step * (double)inNumberFrames;
		return noErr;
	}

	double pos = g_mon_read_pos;
	for (UInt32 i = 0; i < inNumberFrames; i++) {
		uint64_t i0 = (uint64_t)pos;
		if (i0 + 1 >= writePos) {
			g_mon_initialized = 0;
			atomic_fetch_add_explicit(&g_mon_underruns, 1, memory_order_relaxed);
			return noErr;
		}
		float frac = (float)(pos - (double)i0);
		uint64_t idx0 = (i0 & MON_RING_MASK) * 2;
		uint64_t idx1 = ((i0 + 1) & MON_RING_MASK) * 2;

		float l = g_mon_ring[idx0] + (g_mon_ring[idx1] - g_mon_ring[idx0]) * frac;
		float r = g_mon_ring[idx0 + 1] + (g_mon_ring[idx1 + 1] - g_mon_ring[idx0 + 1]) * frac;

		float env = 1.0f;
		if (g_mon_fade_in_rem > 0) {
			env = (float)(240 - g_mon_fade_in_rem) / 240.0f;
			g_mon_fade_in_rem--;
		}
		float g = gain * env;
		out[2 * i] = l * g;
		out[2 * i + 1] = r * g;
		pos += step;
	}
	g_mon_read_pos = pos;
	return noErr;
}

static double query_output_device_latency_ms(AudioDeviceID dev) {
	Float64 sr = TARGET_SAMPLE_RATE;
	UInt32 srSize = sizeof(sr);
	AudioObjectPropertyAddress srAddr = {
		kAudioDevicePropertyNominalSampleRate,
		kAudioObjectPropertyScopeGlobal,
		kAudioObjectPropertyElementMain
	};
	AudioObjectGetPropertyData(dev, &srAddr, 0, NULL, &srSize, &sr);
	if (sr <= 1000.0) sr = TARGET_SAMPLE_RATE;

	UInt32 devLat = 0, safety = 0, bufSize = 0, streamLat = 0;
	UInt32 sz = sizeof(UInt32);

	AudioObjectPropertyAddress latAddr = {kAudioDevicePropertyLatency, kAudioObjectPropertyScopeOutput, kAudioObjectPropertyElementMain};
	AudioObjectGetPropertyData(dev, &latAddr, 0, NULL, &sz, &devLat);

	sz = sizeof(UInt32);
	AudioObjectPropertyAddress safAddr = {kAudioDevicePropertySafetyOffset, kAudioObjectPropertyScopeOutput, kAudioObjectPropertyElementMain};
	AudioObjectGetPropertyData(dev, &safAddr, 0, NULL, &sz, &safety);

	sz = sizeof(UInt32);
	AudioObjectPropertyAddress bufAddr = {kAudioDevicePropertyBufferFrameSize, kAudioObjectPropertyScopeOutput, kAudioObjectPropertyElementMain};
	AudioObjectGetPropertyData(dev, &bufAddr, 0, NULL, &sz, &bufSize);

	AudioObjectPropertyAddress stAddr = {kAudioDevicePropertyStreams, kAudioObjectPropertyScopeOutput, kAudioObjectPropertyElementMain};
	UInt32 stSz = 0;
	if (AudioObjectGetPropertyDataSize(dev, &stAddr, 0, NULL, &stSz) == noErr && stSz >= sizeof(AudioStreamID)) {
		AudioStreamID *streams = (AudioStreamID *)malloc(stSz);
		if (streams) {
			if (AudioObjectGetPropertyData(dev, &stAddr, 0, NULL, &stSz, streams) == noErr && stSz >= sizeof(AudioStreamID)) {
				UInt32 ssz = sizeof(streamLat);
				AudioObjectPropertyAddress slAddr = {kAudioStreamPropertyLatency, kAudioObjectPropertyScopeGlobal, kAudioObjectPropertyElementMain};
				AudioObjectGetPropertyData(streams[0], &slAddr, 0, NULL, &ssz, &streamLat);
			}
			free(streams);
		}
	}

	double totalFrames = (double)(devLat + safety + bufSize + streamLat);
	return totalFrames * 1000.0 / (double)sr;
}

void ss_stop_monitor(void) {
	if (g_mon_unit) {
		AudioOutputUnitStop(g_mon_unit);
		AudioUnitUninitialize(g_mon_unit);
		AudioComponentInstanceDispose(g_mon_unit);
		g_mon_unit = NULL;
	}
	mon_ring_reset();
}

int ss_start_monitor(uint32_t output_device_id, double *out_latency_ms, char *err_buf, int err_buf_len) {
	ss_stop_monitor();
	mon_ring_reset();

	AudioDeviceID dev = (output_device_id != 0) ? (AudioDeviceID)output_device_id : get_default_device(kAudioHardwarePropertyDefaultOutputDevice);
	if (dev == kAudioObjectUnknown) {
		set_err(err_buf, err_buf_len, "no output audio device found", noErr);
		return -1;
	}
	if (get_device_channels(dev, kAudioObjectPropertyScopeOutput) <= 0) {
		set_err(err_buf, err_buf_len, "selected monitor device has no output channels", noErr);
		return -1;
	}

	AudioComponentDescription desc = {0};
	desc.componentType = kAudioUnitType_Output;
	desc.componentSubType = kAudioUnitSubType_HALOutput;
	desc.componentManufacturer = kAudioUnitManufacturer_Apple;
	AudioComponent comp = AudioComponentFindNext(NULL, &desc);
	if (!comp) {
		set_err(err_buf, err_buf_len, "HALOutput AudioComponent not found", noErr);
		return -1;
	}

	AudioUnit unit = NULL;
	OSStatus st = AudioComponentInstanceNew(comp, &unit);
	if (st != noErr || !unit) {
		set_err(err_buf, err_buf_len, "AudioComponentInstanceNew failed", st);
		return -1;
	}

	st = AudioUnitSetProperty(unit, kAudioOutputUnitProperty_CurrentDevice, kAudioUnitScope_Global, 0, &dev, sizeof(dev));
	if (st != noErr) {
		AudioComponentInstanceDispose(unit);
		set_err(err_buf, err_buf_len, "failed to set HALOutput device", st);
		return -1;
	}

	AudioStreamBasicDescription asbd = {0};
	asbd.mSampleRate = TARGET_SAMPLE_RATE;
	asbd.mFormatID = kAudioFormatLinearPCM;
	asbd.mFormatFlags = kAudioFormatFlagIsFloat | kAudioFormatFlagIsPacked;
	asbd.mFramesPerPacket = 1;
	asbd.mChannelsPerFrame = 2;
	asbd.mBitsPerChannel = 32;
	asbd.mBytesPerFrame = 8;
	asbd.mBytesPerPacket = 8;

	st = AudioUnitSetProperty(unit, kAudioUnitProperty_StreamFormat, kAudioUnitScope_Input, 0, &asbd, sizeof(asbd));
	if (st != noErr) {
		AudioComponentInstanceDispose(unit);
		set_err(err_buf, err_buf_len, "failed to set HALOutput stream format", st);
		return -1;
	}

	AURenderCallbackStruct cb = { monitor_render_cb, NULL };
	st = AudioUnitSetProperty(unit, kAudioUnitProperty_SetRenderCallback, kAudioUnitScope_Input, 0, &cb, sizeof(cb));
	if (st != noErr) {
		AudioComponentInstanceDispose(unit);
		set_err(err_buf, err_buf_len, "failed to set HALOutput render callback", st);
		return -1;
	}

	st = AudioUnitInitialize(unit);
	if (st != noErr) {
		AudioComponentInstanceDispose(unit);
		set_err(err_buf, err_buf_len, "AudioUnitInitialize failed", st);
		return -1;
	}

	st = AudioOutputUnitStart(unit);
	if (st != noErr) {
		AudioUnitUninitialize(unit);
		AudioComponentInstanceDispose(unit);
		set_err(err_buf, err_buf_len, "AudioOutputUnitStart failed", st);
		return -1;
	}

	g_mon_unit = unit;
	if (out_latency_ms) {
		*out_latency_ms = query_output_device_latency_ms(dev);
	}
	return 0;
}

void ss_monitor_push(const float *interleaved48k, int frames, int64_t capture_ns) {
	if (!interleaved48k || frames <= 0 || !g_mon_unit) return;

	uint64_t w = atomic_load_explicit(&g_mon_write_pos, memory_order_relaxed);
	for (int i = 0; i < frames; i++) {
		uint64_t idx = ((w + (uint64_t)i) & MON_RING_MASK) * 2;
		g_mon_ring[idx] = interleaved48k[2 * i];
		g_mon_ring[idx + 1] = interleaved48k[2 * i + 1];
	}
	uint64_t newW = w + (uint64_t)frames;
	int64_t endNs = capture_ns + ((int64_t)frames * 1000000000LL) / (int64_t)TARGET_SAMPLE_RATE;
	atomic_store_explicit(&g_mon_anchor_frame, newW, memory_order_relaxed);
	atomic_store_explicit(&g_mon_anchor_ns, endNs, memory_order_relaxed);
	atomic_store_explicit(&g_mon_write_pos, newW, memory_order_release);
}

void ss_monitor_set_params(float target_delay_ms, float gain, int muted) {
	if (target_delay_ms < 0.0f) target_delay_ms = 0.0f;
	if (target_delay_ms > 2000.0f) target_delay_ms = 2000.0f;
	if (gain < 0.0f) gain = 0.0f;
	if (gain > 1.0f) gain = 1.0f;
	atomic_store_explicit(&g_mon_target_delay_ms, target_delay_ms, memory_order_relaxed);
	atomic_store_explicit(&g_mon_gain, gain, memory_order_relaxed);
	atomic_store_explicit(&g_mon_muted, muted ? 1 : 0, memory_order_relaxed);
}

void ss_monitor_stats(double *out_actual_delay_ms, uint64_t *out_underruns) {
	if (out_actual_delay_ms) {
		*out_actual_delay_ms = atomic_load_explicit(&g_mon_actual_delay_ms, memory_order_relaxed);
	}
	if (out_underruns) {
		*out_underruns = atomic_load_explicit(&g_mon_underruns, memory_order_relaxed);
	}
}
