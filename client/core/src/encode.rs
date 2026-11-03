//! H.264 encoding with OpenH264 (constrained baseline, Annex B output).
use anyhow::{Result, anyhow};
use openh264::OpenH264API;
use openh264::encoder::{
    BitRate, Complexity, Encoder, EncoderConfig, FrameRate, FrameType, IntraFramePeriod,
    RateControlMode, UsageType,
};
use openh264::formats::{BgraSliceU8, YUVBuffer};

use crate::capture::Frame;

pub struct H264Encoder {
    enc: Encoder,
    yuv: YUVBuffer,
    width: usize,
    height: usize,
    out: Vec<u8>,
}

/// One encoded access unit.
pub struct Encoded {
    pub data: Vec<u8>,
    pub key: bool,
}

/// Bitrate for a resolution at screen-content quality.
pub fn default_bitrate(width: usize, height: usize) -> u32 {
    let px = (width * height) as u64;
    // About 2.5 bits per pixel per second: 1080p ≈ 5 Mbit/s.
    (px * 5 / 2).clamp(1_000_000, 12_000_000) as u32
}

impl H264Encoder {
    pub fn new(width: usize, height: usize, fps: f32, bitrate: u32) -> Result<Self> {
        let cfg = EncoderConfig::new()
            .usage_type(UsageType::ScreenContentRealTime)
            .bitrate(BitRate::from_bps(bitrate))
            .max_frame_rate(FrameRate::from_hz(fps))
            .rate_control_mode(RateControlMode::Bitrate)
            .complexity(Complexity::Low)
            // A key frame every 4 seconds lets a viewer recover from lost packets.
            .intra_frame_period(IntraFramePeriod::from_num_frames((fps * 4.0) as u32))
            .skip_frames(true);
        let enc = Encoder::with_api_config(OpenH264API::from_source(), cfg)
            .map_err(|e| anyhow!("openh264: {e}"))?;
        Ok(Self {
            enc,
            yuv: YUVBuffer::new(width, height),
            width,
            height,
            out: Vec::new(),
        })
    }

    pub fn size(&self) -> (usize, usize) {
        (self.width, self.height)
    }

    /// Encodes a frame; None when the encoder skipped it (rate control).
    pub fn encode(&mut self, frame: &Frame, force_key: bool) -> Result<Option<Encoded>> {
        if frame.width != self.width || frame.height != self.height {
            return Err(anyhow!("frame size changed"));
        }
        self.yuv
            .read_bgra8(BgraSliceU8::new(&frame.data, (frame.width, frame.height)));
        if force_key {
            self.enc.force_intra_frame();
        }
        let bs = self
            .enc
            .encode(&self.yuv)
            .map_err(|e| anyhow!("encode: {e}"))?;
        let ft = bs.frame_type();
        if matches!(ft, FrameType::Skip | FrameType::Invalid) {
            return Ok(None);
        }
        self.out.clear();
        bs.write_vec(&mut self.out);
        if self.out.is_empty() {
            return Ok(None);
        }
        let key = matches!(ft, FrameType::IDR) || has_idr(&self.out);
        Ok(Some(Encoded {
            data: self.out.clone(),
            key,
        }))
    }
}

/// Whether an Annex B access unit contains an IDR slice (NAL type 5).
pub fn has_idr(au: &[u8]) -> bool {
    let mut i = 0;
    while i + 3 < au.len() {
        if au[i] == 0 && au[i + 1] == 0 && au[i + 2] == 1 {
            if au[i + 3] & 0x1f == 5 {
                return true;
            }
            i += 3;
        } else {
            i += 1;
        }
    }
    false
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn encodes_key_frame_then_delta() {
        let (w, h) = (320, 240);
        let mut enc = H264Encoder::new(w, h, 30.0, 1_000_000).unwrap();
        let mut f = Frame {
            width: w,
            height: h,
            data: vec![0u8; w * h * 4],
        };
        let first = enc.encode(&f, false).unwrap().expect("first frame");
        assert!(first.key);
        assert_eq!(&first.data[..4], &[0, 0, 0, 1], "Annex B start code");
        // A small change: one block of pixels (a big change can trigger a scene-change key frame).
        for px in f.data[..64 * 4].chunks_mut(4) {
            px[0] = 200;
        }
        let mut delta = None;
        for _ in 0..3 {
            if let Some(e) = enc.encode(&f, false).unwrap() {
                delta = Some(e);
                break;
            }
        }
        assert!(!delta.expect("second frame").key);
        let key = enc.encode(&f, true).unwrap().expect("forced key frame");
        assert!(key.key);
    }
}
