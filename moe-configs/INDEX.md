# MoE Tuned Configs Index

Generated: 2026-03-01T12:06:48+01:00

## Current (recommended for recipes)

- `Intel/Qwen3.5-122B-A10B-int4-AutoRound` (`TP=2`, `EP=off`):
  - Source of truth:
    - `/home/csolutions_ai/swap-laboratories/moe-configs/current/Intel_Qwen3.5-122B-A10B-int4-AutoRound/tp2_epoff/E=256,N=512,device_name=NVIDIA_GB10,dtype=int4_w4a16.json`
  - Batch keys:
    - `[1, 2, 4, 8, 16, 24, 32, 48, 64, 96, 128, 256, 512, 1024, 2048, 4096]`
  - Default container install path used by mod:
    - `/root/.cache/huggingface/moe_tuned_qwen35_tp2_int4_ar_current_v1`

## Qwen_Qwen3.5-122B-A10B-FP8 / tp2_epoff_bs2048_v2

- `E=256,N=512,device_name=NVIDIA_GB10,dtype=fp8_w8a8,block_shape=[128,128].json`: batch keys `[2048]`

## Qwen_Qwen3.5-122B-A10B-FP8 / tp2_epon_bs2048_v1

- `E=128,N=1024,device_name=NVIDIA_GB10,dtype=fp8_w8a8,block_shape=[128,128].json`: batch keys `[2048]`

## Intel_Qwen3.5-122B-A10B-int4-AutoRound / tp2_epoff_bs2048_4096_v1

- `E=256,N=512,device_name=NVIDIA_GB10,dtype=int4_w4a16.json`: batch keys `[2048, 4096]`

## Intel_Qwen3.5-122B-A10B-int4-AutoRound / tp2_epoff_bs2048_4096_8192_v1

- `E=256,N=512,device_name=NVIDIA_GB10,dtype=int4_w4a16.json`: batch keys `[2048, 4096]`
- `E=256,N=512,device_name=NVIDIA_GB10,dtype=int4_w4a16.json.batch_2048.progress.json`: progress batch `2048` -> `1920/1920`
- `E=256,N=512,device_name=NVIDIA_GB10,dtype=int4_w4a16.json.batch_4096.progress.json`: progress batch `4096` -> `1920/1920`
- `E=256,N=512,device_name=NVIDIA_GB10,dtype=int4_w4a16.json.batch_8192.progress.json`: progress batch `8192` -> `384/1920`

## Intel_Qwen3.5-122B-A10B-int4-AutoRound / tp2_epoff_fullgrid_8192_v1_inprogress

- `E=256,N=512,device_name=NVIDIA_GB10,dtype=int4_w4a16.json`: batch keys `[1, 2, 4, 8, 16, 24, 32, 48, 64, 96, 128, 256, 512, 1024, 2048, 4096]`
- `E=256,N=512,device_name=NVIDIA_GB10,dtype=int4_w4a16.json.batch_1.progress.json`: progress batch `1` -> `1920/1920`
- `E=256,N=512,device_name=NVIDIA_GB10,dtype=int4_w4a16.json.batch_1024.progress.json`: progress batch `1024` -> `1920/1920`
- `E=256,N=512,device_name=NVIDIA_GB10,dtype=int4_w4a16.json.batch_128.progress.json`: progress batch `128` -> `1920/1920`
- `E=256,N=512,device_name=NVIDIA_GB10,dtype=int4_w4a16.json.batch_1536.progress.json`: progress batch `1536` -> `864/1920`
- `E=256,N=512,device_name=NVIDIA_GB10,dtype=int4_w4a16.json.batch_16.progress.json`: progress batch `16` -> `1920/1920`
- `E=256,N=512,device_name=NVIDIA_GB10,dtype=int4_w4a16.json.batch_2.progress.json`: progress batch `2` -> `1920/1920`
- `E=256,N=512,device_name=NVIDIA_GB10,dtype=int4_w4a16.json.batch_2048.progress.json`: progress batch `2048` -> `1920/1920`
- `E=256,N=512,device_name=NVIDIA_GB10,dtype=int4_w4a16.json.batch_24.progress.json`: progress batch `24` -> `1920/1920`
- `E=256,N=512,device_name=NVIDIA_GB10,dtype=int4_w4a16.json.batch_256.progress.json`: progress batch `256` -> `1920/1920`
- `E=256,N=512,device_name=NVIDIA_GB10,dtype=int4_w4a16.json.batch_32.progress.json`: progress batch `32` -> `1920/1920`
- `E=256,N=512,device_name=NVIDIA_GB10,dtype=int4_w4a16.json.batch_4.progress.json`: progress batch `4` -> `1920/1920`
- `E=256,N=512,device_name=NVIDIA_GB10,dtype=int4_w4a16.json.batch_4096.progress.json`: progress batch `4096` -> `1920/1920`
- `E=256,N=512,device_name=NVIDIA_GB10,dtype=int4_w4a16.json.batch_48.progress.json`: progress batch `48` -> `1920/1920`
- `E=256,N=512,device_name=NVIDIA_GB10,dtype=int4_w4a16.json.batch_512.progress.json`: progress batch `512` -> `1920/1920`
- `E=256,N=512,device_name=NVIDIA_GB10,dtype=int4_w4a16.json.batch_64.progress.json`: progress batch `64` -> `1920/1920`
- `E=256,N=512,device_name=NVIDIA_GB10,dtype=int4_w4a16.json.batch_8.progress.json`: progress batch `8` -> `1920/1920`
- `E=256,N=512,device_name=NVIDIA_GB10,dtype=int4_w4a16.json.batch_8192.progress.json`: progress batch `8192` -> `384/1920`
- `E=256,N=512,device_name=NVIDIA_GB10,dtype=int4_w4a16.json.batch_96.progress.json`: progress batch `96` -> `1920/1920`
