//! Inspect the same native byte snapshot that is decoded. In particular, never
//! call clipboard-rs::get_image: it allocates pixels before exposing dimensions.
use crate::remote::{Error, Result};
use image::{DynamicImage, ImageDecoder, ImageEncoder, ImageFormat, ImageReader, Limits};
use std::io::{self, BufRead, Cursor, Read, Seek, SeekFrom, Write};

const MAX_SOURCE_BYTES: usize = 128 * 1024 * 1024;
const MAX_PIXELS: u64 = 32 * 1024 * 1024;
const MAX_DIMENSION: u32 = 32768;
const MAX_DECODED_BYTES: u64 = 128 * 1024 * 1024;

#[derive(Clone, Copy)]
enum Format {
    Png,
    #[cfg_attr(not(any(target_os = "macos", test)), allow(dead_code))]
    Tiff,
    #[cfg_attr(not(any(windows, test)), allow(dead_code))]
    Dib,
}

fn invalid() -> Error {
    Error::new(
        "clipboard",
        "剪贴板图片格式无效或不受支持，请保存为 PNG 后选择文件",
    )
}
fn limit() -> Error {
    Error::new("limit", "剪贴板图片尺寸或内容过大，请裁剪后重试")
}
fn image_error(error: image::ImageError) -> Error {
    match error {
        image::ImageError::Limits(_) => limit(),
        _ => invalid(),
    }
}
fn check_source_len(len: usize) -> Result<()> {
    if len > MAX_SOURCE_BYTES {
        return Err(limit());
    }
    if len == 0 {
        return Err(invalid());
    }
    Ok(())
}
fn check_dimensions(width: u32, height: u32) -> Result<()> {
    if width == 0 || height == 0 {
        return Err(invalid());
    }
    if width > MAX_DIMENSION
        || height > MAX_DIMENSION
        || u64::from(width) * u64::from(height) > MAX_PIXELS
    {
        return Err(limit());
    }
    Ok(())
}
fn u16_at(bytes: &[u8], offset: usize, little: bool) -> Result<u16> {
    let value = bytes
        .get(offset..offset.checked_add(2).ok_or_else(invalid)?)
        .ok_or_else(invalid)?;
    Ok(if little {
        u16::from_le_bytes([value[0], value[1]])
    } else {
        u16::from_be_bytes([value[0], value[1]])
    })
}
fn u32_at(bytes: &[u8], offset: usize, little: bool) -> Result<u32> {
    let value = bytes
        .get(offset..offset.checked_add(4).ok_or_else(invalid)?)
        .ok_or_else(invalid)?;
    let value = [value[0], value[1], value[2], value[3]];
    Ok(if little {
        u32::from_le_bytes(value)
    } else {
        u32::from_be_bytes(value)
    })
}

// This reads only fixed-size fields, before constructing a decoder. Classic
// TIFF is enough for native screenshots; BigTIFF and ambiguous duplicate size
// tags are deliberately refused. The decoder subsequently validates the format.
fn dimensions(bytes: &[u8], format: Format) -> Result<(u32, u32)> {
    check_source_len(bytes.len())?;
    let size = match format {
        Format::Png => {
            if !bytes.starts_with(b"\x89PNG\r\n\x1a\n\0\0\0\rIHDR") {
                return Err(invalid());
            }
            (u32_at(bytes, 16, false)?, u32_at(bytes, 20, false)?)
        }
        Format::Dib => match u32_at(bytes, 0, true)? {
            12 => (
                u32::from(u16_at(bytes, 4, true)?),
                u32::from(u16_at(bytes, 6, true)?),
            ),
            40 | 52 | 56 | 108 | 124 => {
                let width = u32_at(bytes, 4, true)? as i32;
                let height = u32_at(bytes, 8, true)? as i32;
                if width <= 0 || height == i32::MIN {
                    return Err(invalid());
                }
                (width as u32, height.unsigned_abs())
            }
            _ => return Err(invalid()),
        },
        Format::Tiff => {
            let little = match bytes.get(..4) {
                Some(b"II\x2a\0") => true,
                Some(b"MM\0\x2a") => false,
                _ => return Err(invalid()),
            };
            let offset = u32_at(bytes, 4, little)? as usize;
            let count = u16_at(bytes, offset, little)? as usize;
            if count > 256 {
                return Err(limit());
            }
            let start = offset.checked_add(2).ok_or_else(invalid)?;
            let end = start.checked_add(count * 12).ok_or_else(invalid)?;
            let entries = bytes.get(start..end).ok_or_else(invalid)?;
            let mut width = None;
            let mut height = None;
            let mut metadata_bytes = 0_u64;
            for entry in entries.chunks_exact(12) {
                let tag = u16_at(entry, 0, little)?;
                let kind = u16_at(entry, 2, little)?;
                let count = u32_at(entry, 4, little)?;
                // Bound header work/allocations as well as pixel storage. No
                // TIFF tag may point outside this already bounded snapshot.
                let field_size = match kind {
                    1 | 2 | 6 | 7 => 1,
                    3 | 8 => 2,
                    4 | 9 | 11 | 13 => 4,
                    5 | 10 | 12 => 8,
                    _ => return Err(invalid()),
                };
                let field_bytes = u64::from(count) * field_size;
                metadata_bytes += field_bytes;
                if metadata_bytes > 4 * 1024 * 1024 {
                    return Err(limit());
                }
                if field_bytes > 4 {
                    let value_offset = u64::from(u32_at(entry, 8, little)?);
                    if value_offset + field_bytes > bytes.len() as u64 {
                        return Err(invalid());
                    }
                }
                if tag != 256 && tag != 257 {
                    continue;
                }
                if count != 1 {
                    return Err(invalid());
                }
                let value = match kind {
                    3 => u32::from(u16_at(entry, 8, little)?),
                    4 => u32_at(entry, 8, little)?,
                    _ => return Err(invalid()),
                };
                let target = if tag == 256 { &mut width } else { &mut height };
                if target.replace(value).is_some() {
                    return Err(invalid());
                }
            }
            (width.ok_or_else(invalid)?, height.ok_or_else(invalid)?)
        }
    };
    check_dimensions(size.0, size.1)?;
    Ok(size)
}

// Packed clipboard DIBs contain no BITMAPFILEHEADER. Supply its exact pixel
// offset rather than asking image 0.25.10 to infer it: that decoder incorrectly
// skips an additional 12 bytes after V4/V5 BI_BITFIELDS headers, whose masks
// are already inside the header. Only uncompressed screenshot layouts are
// supported; a color profile must follow the pixels in a packed DIB.
fn dib_pixel_offset(bytes: &[u8], dimensions: (u32, u32)) -> Result<usize> {
    let header = u32_at(bytes, 0, true)? as usize;
    if !matches!(header, 12 | 40 | 52 | 56 | 108 | 124) || bytes.len() < header {
        return Err(invalid());
    }
    let (bits, colors, masks, palette_entry) = if header == 12 {
        if u16_at(bytes, 8, true)? != 1 {
            return Err(invalid());
        }
        let bits = u16_at(bytes, 10, true)?;
        if !matches!(bits, 1 | 4 | 8 | 24) {
            return Err(invalid());
        }
        (bits, 0, 0, 3)
    } else {
        if u16_at(bytes, 12, true)? != 1 {
            return Err(invalid());
        }
        let bits = u16_at(bytes, 14, true)?;
        let compression = u32_at(bytes, 16, true)?;
        if !matches!(bits, 1 | 4 | 8 | 16 | 24 | 32)
            || !matches!(compression, 0 | 3)
            || compression == 3 && !matches!(bits, 16 | 32)
        {
            return Err(invalid());
        }
        (
            bits,
            u32_at(bytes, 32, true)?,
            if header == 40 && compression == 3 {
                12
            } else {
                0
            },
            4,
        )
    };
    let palette = if bits <= 8 && colors == 0 {
        1_u32 << bits
    } else {
        colors
    };
    if palette > 256 || bits <= 8 && palette > 1_u32 << bits {
        return Err(invalid());
    }
    let offset = header + masks + palette as usize * palette_entry;
    let row = (u64::from(dimensions.0) * u64::from(bits)).div_ceil(32) * 4;
    let pixel_end = offset as u64 + row * u64::from(dimensions.1);
    if pixel_end > bytes.len() as u64 {
        return Err(invalid());
    }
    if header == 124 {
        let profile = u64::from(u32_at(bytes, 112, true)?);
        let size = u64::from(u32_at(bytes, 116, true)?);
        if (profile != 0 || size != 0)
            && (profile < pixel_end || size == 0 || profile + size > bytes.len() as u64)
        {
            return Err(invalid());
        }
    }
    Ok(offset)
}

// A seekable virtual BMP adds 14 bytes without duplicating the bounded native
// snapshot. Decoder reads and seeks share this same immutable byte source.
struct DibReader<'a> {
    header: [u8; 14],
    bytes: &'a [u8],
    position: u64,
}
impl<'a> DibReader<'a> {
    fn new(bytes: &'a [u8], dimensions: (u32, u32)) -> Result<Self> {
        let offset = dib_pixel_offset(bytes, dimensions)?;
        let mut header = [0; 14];
        header[..2].copy_from_slice(b"BM");
        header[2..6].copy_from_slice(&((bytes.len() + 14) as u32).to_le_bytes());
        header[10..14].copy_from_slice(&((offset + 14) as u32).to_le_bytes());
        Ok(Self {
            header,
            bytes,
            position: 0,
        })
    }
}
impl BufRead for DibReader<'_> {
    fn fill_buf(&mut self) -> io::Result<&[u8]> {
        if self.position < 14 {
            Ok(&self.header[self.position as usize..])
        } else if self.position - 14 < self.bytes.len() as u64 {
            Ok(&self.bytes[(self.position - 14) as usize..])
        } else {
            Ok(&[])
        }
    }
    fn consume(&mut self, amount: usize) {
        self.position = self.position.saturating_add(amount as u64);
    }
}
impl Read for DibReader<'_> {
    fn read(&mut self, destination: &mut [u8]) -> io::Result<usize> {
        let source = self.fill_buf()?;
        let size = destination.len().min(source.len());
        destination[..size].copy_from_slice(&source[..size]);
        self.consume(size);
        Ok(size)
    }
}
impl Seek for DibReader<'_> {
    fn seek(&mut self, position: SeekFrom) -> io::Result<u64> {
        self.position = match position {
            SeekFrom::Start(offset) => Some(offset),
            SeekFrom::Current(offset) => self.position.checked_add_signed(offset),
            SeekFrom::End(offset) => (self.bytes.len() as u64 + 14).checked_add_signed(offset),
        }
        .ok_or_else(|| io::Error::new(io::ErrorKind::InvalidInput, "invalid DIB seek"))?;
        Ok(self.position)
    }
}

fn decode(bytes: &[u8], format: Format) -> Result<Vec<u8>> {
    let expected = dimensions(bytes, format)?;
    let mut limits = Limits::default();
    limits.max_image_width = Some(MAX_DIMENSION);
    limits.max_image_height = Some(MAX_DIMENSION);
    // This library limit is best-effort, not a promise about total process RSS.
    // Encoded bytes, dimensions and decoded output bytes are also checked here.
    limits.max_alloc = Some(MAX_DECODED_BYTES);
    let image = match format {
        Format::Dib => {
            let mut decoder = image::codecs::bmp::BmpDecoder::new(DibReader::new(bytes, expected)?)
                .map_err(image_error)?;
            decoder.set_limits(limits).map_err(image_error)?;
            decode_checked(decoder, expected)?
        }
        Format::Png | Format::Tiff => {
            let mut reader = ImageReader::with_format(
                Cursor::new(bytes),
                match format {
                    Format::Png => ImageFormat::Png,
                    _ => ImageFormat::Tiff,
                },
            );
            reader.limits(limits);
            decode_checked(reader.into_decoder().map_err(image_error)?, expected)?
        }
    };
    encode(image, super::MAX_BYTES as usize)
}

fn decode_checked(decoder: impl ImageDecoder, expected: (u32, u32)) -> Result<DynamicImage> {
    if decoder.dimensions() != expected {
        return Err(invalid());
    }
    if decoder.total_bytes() > MAX_DECODED_BYTES {
        return Err(limit());
    }
    DynamicImage::from_decoder(decoder).map_err(image_error)
}

struct BoundedOutput {
    bytes: Vec<u8>,
    max: usize,
    exceeded: bool,
}
impl Write for BoundedOutput {
    fn write(&mut self, data: &[u8]) -> io::Result<usize> {
        if data.len() > self.max.saturating_sub(self.bytes.len()) {
            self.exceeded = true;
            return Err(io::Error::other("clipboard PNG exceeds limit"));
        }
        // Avoid Vec's geometric growth allocating beyond the output cap.
        self.bytes
            .try_reserve_exact(data.len())
            .map_err(io::Error::other)?;
        self.bytes.extend_from_slice(data);
        Ok(data.len())
    }
    fn flush(&mut self) -> io::Result<()> {
        Ok(())
    }
}
fn encode(image: DynamicImage, max: usize) -> Result<Vec<u8>> {
    let rgba = image.into_rgba8();
    let mut output = BoundedOutput {
        bytes: Vec::new(),
        max,
        exceeded: false,
    };
    let encoded = image::codecs::png::PngEncoder::new(&mut output).write_image(
        rgba.as_raw(),
        rgba.width(),
        rgba.height(),
        image::ExtendedColorType::Rgba8,
    );
    if output.exceeded {
        return Err(Error::new(
            "limit",
            "剪贴板图片转换后超过 19 MiB，请裁剪后重试",
        ));
    }
    encoded.map_err(image_error)?;
    Ok(output.bytes)
}

#[cfg(target_os = "macos")]
pub(super) fn read() -> Result<Option<Vec<u8>>> {
    use objc2_app_kit::{NSPasteboard, NSPasteboardTypePNG, NSPasteboardTypeTIFF};
    objc2::rc::autoreleasepool(|_| {
        let pasteboard = NSPasteboard::generalPasteboard();
        let revision = pasteboard.changeCount();
        // Reading NSData can materialize a promised representation inside the
        // OS. Its size is checked before our copy or any image decode. Do not
        // use NSImage/TIFFRepresentation, which may rasterize before this check.
        for (kind, format) in [
            (unsafe { NSPasteboardTypePNG }, Format::Png),
            (unsafe { NSPasteboardTypeTIFF }, Format::Tiff),
        ] {
            if let Some(data) = pasteboard.dataForType(kind) {
                check_source_len(data.length())?;
                let snapshot = data.to_vec();
                if pasteboard.changeCount() != revision {
                    return Err(Error::new("changed", "剪贴板已变化，请重新粘贴"));
                }
                return decode(&snapshot, format).map(Some);
            }
        }
        Ok(None)
    })
}

#[cfg(windows)]
pub(super) fn read() -> Result<Option<Vec<u8>>> {
    use windows_sys::Win32::System::{
        DataExchange::{
            CloseClipboard, GetClipboardData, IsClipboardFormatAvailable, OpenClipboard,
            RegisterClipboardFormatW,
        },
        Memory::{GlobalLock, GlobalSize, GlobalUnlock},
    };
    struct ClipboardGuard;
    impl Drop for ClipboardGuard {
        fn drop(&mut self) {
            unsafe {
                CloseClipboard();
            }
        }
    }
    struct MemoryGuard(windows_sys::Win32::Foundation::HGLOBAL);
    impl Drop for MemoryGuard {
        fn drop(&mut self) {
            unsafe {
                GlobalUnlock(self.0);
            }
        }
    }
    let snapshot = {
        if unsafe { OpenClipboard(std::ptr::null_mut()) } == 0 {
            return Err(Error::new("clipboard", "系统剪贴板正忙，请重试"));
        }
        let _clipboard = ClipboardGuard;
        let png = unsafe { RegisterClipboardFormatW([80_u16, 78, 71, 0].as_ptr()) };
        if png == 0 {
            return Err(invalid());
        }
        let mut result = None;
        // CF_DIBV5 = 17, CF_DIB = 8. Both expose a memory block without the
        // BITMAPFILEHEADER; retaining the clipboard lock pins that block.
        for (kind, format) in [(png, Format::Png), (17, Format::Dib), (8, Format::Dib)] {
            if unsafe { IsClipboardFormatAvailable(kind) } == 0 {
                continue;
            }
            let handle = unsafe { GetClipboardData(kind) };
            if handle.is_null() {
                return Err(invalid());
            }
            let len = unsafe { GlobalSize(handle) };
            check_source_len(len)?;
            let pointer = unsafe { GlobalLock(handle) };
            if pointer.is_null() {
                return Err(invalid());
            }
            let _memory = MemoryGuard(handle);
            // GlobalSize is bounded before forming a slice. The clipboard and
            // global memory remain locked until the one snapshot copy finishes.
            let bytes = unsafe { std::slice::from_raw_parts(pointer.cast::<u8>(), len) };
            dimensions(bytes, format)?;
            result = Some((bytes.to_vec(), format));
            break;
        }
        result
    };
    // Release the OS clipboard before spending time decoding/encoding pixels.
    snapshot
        .map(|(bytes, format)| decode(&bytes, format))
        .transpose()
}

#[cfg(not(any(windows, target_os = "macos")))]
pub(super) fn read() -> Result<Option<Vec<u8>>> {
    use clipboard_rs::{Clipboard, ClipboardContext, ContentFormat};
    let clipboard = ClipboardContext::new().map_err(|_| invalid())?;
    if clipboard.has(ContentFormat::Image) {
        // Production desktop targets are macOS and Windows. Do not fall back
        // to an unbounded image decoder on an unsupported native backend.
        return Err(Error::new(
            "clipboard",
            "此系统暂不支持图片粘贴，请选择图片文件",
        ));
    }
    Ok(None)
}

#[cfg(test)]
mod tests {
    use super::*;

    fn png(width: u32, height: u32) -> Vec<u8> {
        encode(DynamicImage::new_rgba8(width, height), 1024 * 1024).unwrap()
    }
    fn tiff_header(little: bool, width: u32, height: u32) -> Vec<u8> {
        let mut bytes = if little {
            b"II\x2a\0".to_vec()
        } else {
            b"MM\0\x2a".to_vec()
        };
        fn word(bytes: &mut Vec<u8>, value: u16, little: bool) {
            bytes.extend_from_slice(&if little {
                value.to_le_bytes()
            } else {
                value.to_be_bytes()
            });
        }
        fn long(bytes: &mut Vec<u8>, value: u32, little: bool) {
            bytes.extend_from_slice(&if little {
                value.to_le_bytes()
            } else {
                value.to_be_bytes()
            });
        }
        long(&mut bytes, 8, little);
        word(&mut bytes, 2, little);
        for (tag, value) in [(256, width), (257, height)] {
            word(&mut bytes, tag, little);
            word(&mut bytes, 4, little);
            long(&mut bytes, 1, little);
            long(&mut bytes, value, little);
        }
        long(&mut bytes, 0, little);
        bytes
    }

    #[test]
    fn png_pixels_survive_bounded_roundtrip() {
        let original = DynamicImage::ImageRgba8(image::RgbaImage::from_fn(3, 2, |x, y| {
            image::Rgba([x as u8 * 60, y as u8 * 80, 150, 100])
        }));
        let source = encode(original.clone(), 1024).unwrap();
        let output = decode(&source, Format::Png).unwrap();
        assert_eq!(
            image::load_from_memory(&output).unwrap().into_rgba8(),
            original.into_rgba8()
        );
    }

    #[test]
    fn oversized_or_zero_png_header_is_rejected_before_crc_or_pixels() {
        for (width, height, kind) in [
            (u32::MAX, u32::MAX, "limit"),
            (8192, 8192, "limit"),
            (32769, 1, "limit"),
            (0, 2, "clipboard"),
        ] {
            let mut bytes = png(1, 1);
            bytes[16..20].copy_from_slice(&width.to_be_bytes());
            bytes[20..24].copy_from_slice(&height.to_be_bytes());
            bytes.truncate(24); // No CRC or compressed data: preflight must win.
            assert_eq!(decode(&bytes, Format::Png).unwrap_err().kind, kind);
        }
        assert!(check_dimensions(8192, 4096).is_ok());
        assert!(check_source_len(MAX_SOURCE_BYTES).is_ok());
        assert_eq!(
            check_source_len(MAX_SOURCE_BYTES + 1).unwrap_err().kind,
            "limit"
        );
    }

    #[test]
    fn truncated_and_corrupt_png_never_become_attachments() {
        let mut bytes = png(2, 2);
        assert!(decode(&bytes[..24], Format::Png).is_err());
        bytes[29] ^= 1; // IHDR CRC.
        assert!(decode(&bytes, Format::Png).is_err());
    }

    #[test]
    fn tiff_preflight_handles_endianness_and_refuses_huge_or_ambiguous_tags() {
        for little in [false, true] {
            assert_eq!(
                dimensions(&tiff_header(little, 3, 2), Format::Tiff).unwrap(),
                (3, 2)
            );
            assert_eq!(
                decode(&tiff_header(little, 8192, 8192), Format::Tiff)
                    .unwrap_err()
                    .kind,
                "limit"
            );
        }
        let mut duplicate = tiff_header(true, 2, 2);
        duplicate[22..24].copy_from_slice(&256_u16.to_le_bytes());
        assert!(dimensions(&duplicate, Format::Tiff).is_err());
        let mut oversized_value = tiff_header(true, 2, 2);
        oversized_value[14..18].copy_from_slice(&u32::MAX.to_le_bytes());
        assert_eq!(
            dimensions(&oversized_value, Format::Tiff).unwrap_err().kind,
            "limit"
        );
        assert!(dimensions(b"II\x2b\0\x08\0\0\0", Format::Tiff).is_err());
    }

    #[test]
    fn native_tiff_and_dib_formats_roundtrip_through_png() {
        let source = DynamicImage::ImageRgb8(image::RgbImage::from_fn(2, 2, |x, y| {
            image::Rgb([x as u8 * 70, y as u8 * 80, 90])
        }));
        for (encoded_format, native_format) in [
            (ImageFormat::Tiff, Format::Tiff),
            (ImageFormat::Bmp, Format::Dib),
        ] {
            let mut encoded = Cursor::new(Vec::new());
            source.write_to(&mut encoded, encoded_format).unwrap();
            let bytes = encoded.into_inner();
            let native = if matches!(native_format, Format::Dib) {
                &bytes[14..]
            } else {
                &bytes[..]
            };
            let output = decode(native, native_format).unwrap();
            assert_eq!(
                image::load_from_memory(&output).unwrap().into_rgb8(),
                source.to_rgb8()
            );
        }
    }

    #[test]
    fn dib_preflight_checks_signed_height_and_large_dimensions() {
        let mut bytes = vec![0; 40];
        bytes[..4].copy_from_slice(&40_u32.to_le_bytes());
        bytes[4..8].copy_from_slice(&3_i32.to_le_bytes());
        bytes[8..12].copy_from_slice(&(-2_i32).to_le_bytes());
        assert_eq!(dimensions(&bytes, Format::Dib).unwrap(), (3, 2));
        bytes[8..12].copy_from_slice(&i32::MIN.to_le_bytes());
        assert!(dimensions(&bytes, Format::Dib).is_err());
        bytes[4..8].copy_from_slice(&8192_u32.to_le_bytes());
        bytes[8..12].copy_from_slice(&8192_u32.to_le_bytes());
        assert_eq!(decode(&bytes, Format::Dib).unwrap_err().kind, "limit");
    }

    fn standard_bitfield_dib(header: usize) -> Vec<u8> {
        let masks = if header == 40 { 12 } else { 0 };
        let mut bytes = vec![0; header + masks];
        bytes[..4].copy_from_slice(&(header as u32).to_le_bytes());
        bytes[4..8].copy_from_slice(&2_i32.to_le_bytes());
        bytes[8..12].copy_from_slice(&(-2_i32).to_le_bytes());
        bytes[12..14].copy_from_slice(&1_u16.to_le_bytes());
        bytes[14..16].copy_from_slice(&32_u16.to_le_bytes());
        bytes[16..20].copy_from_slice(&3_u32.to_le_bytes()); // BI_BITFIELDS.
        bytes[20..24].copy_from_slice(&16_u32.to_le_bytes());
        bytes[40..44].copy_from_slice(&0x00ff0000_u32.to_le_bytes());
        bytes[44..48].copy_from_slice(&0x0000ff00_u32.to_le_bytes());
        bytes[48..52].copy_from_slice(&0x000000ff_u32.to_le_bytes());
        if header >= 56 {
            bytes[52..56].copy_from_slice(&0xff000000_u32.to_le_bytes());
        }
        // Standard packed layout: masks are external only for the 40-byte
        // header. V4/V5 pixels immediately follow their complete header.
        bytes.extend_from_slice(&[3, 2, 1, 255, 6, 5, 4, 128, 9, 8, 7, 64, 12, 11, 10, 0]);
        bytes
    }

    #[test]
    fn standard_dib_bitfields_preserve_pixel_offset_top_down_rows_and_alpha() {
        for header in [40, 52, 56, 108, 124] {
            let bytes = standard_bitfield_dib(header);
            let output = decode(&bytes, Format::Dib).unwrap();
            let image = image::load_from_memory(&output).unwrap().into_rgba8();
            let expected = if header >= 56 {
                [1, 2, 3, 255, 4, 5, 6, 128, 7, 8, 9, 64, 10, 11, 12, 0]
            } else {
                [1, 2, 3, 255, 4, 5, 6, 255, 7, 8, 9, 255, 10, 11, 12, 255]
            };
            assert_eq!(image.as_raw(), &expected, "DIB header {header}");
        }
    }

    #[test]
    fn packed_dib_rejects_bad_profiles_palettes_compression_and_truncated_pixels() {
        let mut bytes = standard_bitfield_dib(124);
        let profile = bytes.len() as u32;
        bytes[112..116].copy_from_slice(&profile.to_le_bytes());
        bytes[116..120].copy_from_slice(&4_u32.to_le_bytes());
        bytes.extend_from_slice(&[1, 2, 3, 4]);
        assert!(decode(&bytes, Format::Dib).is_ok());
        bytes[112..116].copy_from_slice(&124_u32.to_le_bytes());
        assert!(decode(&bytes, Format::Dib).is_err()); // Profile overlaps pixels.
        let mut bytes = standard_bitfield_dib(40);
        bytes.pop();
        assert!(decode(&bytes, Format::Dib).is_err());
        let mut bytes = standard_bitfield_dib(40);
        bytes[16..20].copy_from_slice(&1_u32.to_le_bytes()); // RLE8 is unsupported.
        assert!(decode(&bytes, Format::Dib).is_err());
        let mut bytes = standard_bitfield_dib(40);
        bytes[32..36].copy_from_slice(&257_u32.to_le_bytes());
        assert!(decode(&bytes, Format::Dib).is_err());
    }

    #[test]
    fn virtual_bmp_reader_preserves_snapshot_across_seeks() {
        let bytes = standard_bitfield_dib(124);
        let mut reader = DibReader::new(&bytes, (2, 2)).unwrap();
        let mut complete = Vec::new();
        reader.read_to_end(&mut complete).unwrap();
        assert_eq!(&complete[..2], b"BM");
        assert_eq!(&complete[14..], &bytes);
        assert_eq!(u32_at(&complete, 10, true).unwrap(), 138);
        assert!(reader.seek(SeekFrom::Start(13)).is_ok());
        let mut crossing = [0; 3];
        reader.read_exact(&mut crossing).unwrap();
        assert_eq!(crossing, complete[13..16]);
        assert!(reader.seek(SeekFrom::End(-2)).is_ok());
        let mut tail = Vec::new();
        reader.read_to_end(&mut tail).unwrap();
        assert_eq!(tail, bytes[bytes.len() - 2..]);
        assert!(reader.seek(SeekFrom::Start(u64::MAX)).is_ok());
        assert!(reader.fill_buf().unwrap().is_empty());
        assert!(reader.seek(SeekFrom::Current(1)).is_err());
        assert!(reader.seek(SeekFrom::Start(0)).is_ok());
        assert!(reader.seek(SeekFrom::Current(-1)).is_err());
    }

    #[test]
    fn png_encoding_stops_when_output_limit_is_reached() {
        assert_eq!(
            encode(DynamicImage::new_rgba8(32, 32), 40)
                .unwrap_err()
                .kind,
            "limit"
        );
        let mut output = BoundedOutput {
            bytes: Vec::new(),
            max: 4,
            exceeded: false,
        };
        output.write_all(&[1, 2, 3, 4]).unwrap();
        assert!(output.write_all(&[5]).is_err());
        assert_eq!(output.bytes, [1, 2, 3, 4]);
        assert_eq!(output.bytes.capacity(), 4);
    }
}
