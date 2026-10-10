# mobipeg

Binaries (not in git) from https://github.com/quatric/mobipeg — an FFmpeg fork whose
`mobiclip` encoder writes 3DS `.moflex` video. relay-admin uses `ffmpeg` here to
encode Revivetendo TV uploads for the 3DS eShop (see relay-admin/videos.go).

Installed: v2.3, mobipeg-linux-x86_64.tar.gz, checked against the release's
SHA256SUMS.txt. To update: download a newer release the same way and replace both files.

## ffmpeg-paired (patched build, used for 3D)

`ffmpeg-paired` is mobipeg v2.3 built with `patches/moflex-3d-pair-audio.patch`.
Stock mobipeg cuts the audio into one chunk per video frame. In a 3D moflex
(Interleave3D, `-mo_layout 0`) the left and right eye are separate frames, so each
chunk held only half a picture's worth of sound (~16 ms) and the 3DS eShop player
stuttered constantly. The patch keeps each left/right pair together so the audio
comes in one chunk per pair (~33 ms), like Nintendo's own files. Video bytes and
timestamps are unchanged; 2D files are unaffected. Confirmed on a real 3DS 2026-10-10.

It is a minimal build: it reads only NUT (raw video + PCM) and moflex, so
relay-admin pipes decoded frames into it from the system ffmpeg. To rebuild
(needs `nasm`):

    git clone https://github.com/quatric/x264 && cd x264
    ./configure --prefix=$PWD/../x264-install --enable-static --enable-pic --disable-cli && make -j && make install
    # unpack mobipeg-v2.3-source.tar.gz, apply the patch, then:
    PKG_CONFIG_PATH=$PWD/../x264-install/lib/pkgconfig ./configure --disable-everything --disable-doc \
      --disable-ffplay --disable-ffprobe --disable-network --disable-autodetect --enable-gpl --enable-libx264 \
      --enable-protocol=file,pipe --enable-demuxer=nut,moflex \
      --enable-decoder=rawvideo,pcm_s16le,mobiclip,adpcm_ima_moflex \
      --enable-encoder=mobiclip,libx264,adpcm_ima_moflex,adpcm_ima_amv,rawvideo,pcm_s16le \
      --enable-muxer=moflex,mo,null,nut --enable-filter=aresample,aformat,scale,format,null,anull,select,tile \
      --enable-swscale --enable-swresample && make -j ffmpeg

(`libx264`, `adpcm_ima_amv` and the `mo` muxer are only there because the mobiclip
encoder, the moflex ADPCM encoder and a muxer helper are compiled under their flags.)
Pipe input must be named `pipe:0` (this build has no plain `-`).
