#ifndef ROOMCHORUS_COREAUDIO_DARWIN_H
#define ROOMCHORUS_COREAUDIO_DARWIN_H

#include <stdint.h>

#ifdef __cplusplus
extern "C" {
#endif

typedef struct {
	uint32_t id;
	char uid[128];
	char name[128];
	double sample_rate;
	int has_input;
	int has_output;
	int is_default_input;
	int is_default_output;
} SSDeviceInfo;

int64_t ss_now_ns(void);

int ss_list_devices(SSDeviceInfo *out, int max_count);

int ss_start_system_tap(int mute_original, uint32_t clock_device_id, char *err_buf, int err_buf_len);
int ss_start_input_device(uint32_t device_id, char *err_buf, int err_buf_len);
void ss_stop_capture(void);
int ss_capture_read(float *dst_interleaved, int frames_wanted, int64_t *out_capture_ns);
uint32_t ss_get_self_process_object_id(void);

int ss_start_monitor(uint32_t output_device_id, double *out_latency_ms, char *err_buf, int err_buf_len);
void ss_stop_monitor(void);
void ss_monitor_push(const float *interleaved48k, int frames, int64_t capture_ns);
void ss_monitor_set_params(float target_delay_ms, float gain, int muted);
void ss_monitor_stats(double *out_actual_delay_ms, uint64_t *out_underruns);

#ifdef __cplusplus
}
#endif

#endif
