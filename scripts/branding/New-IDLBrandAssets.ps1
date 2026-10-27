<#
.SYNOPSIS
    Draws the default IDLifecycle icon and writes every brand file the product uses.

.DESCRIPTION
    The mark: a person inside a lifecycle ring (a circular arrow), white on a rounded square with a blue gradient
    (the web console's accent #1558b0 into its top-bar navy #14233d). One geometry is drawn three ways:
      * logo.svg          - the master artwork (256 x 256 viewBox): web header and login page
      * idlifecycle.ico   - 16, 20, 24, 32, 40, 48, 64, 128 and 256 px (PNG-compressed entries): tray icon, setup and tray
                            windows, shortcuts, browser tab. Sizes up to 32 px use a bolder variant so they stay legible.
      * logo-256.png      - for documents
      * brand.json        - product name and accent colour
    The web console embeds its own copy (internal\web\static\branding\logo.svg and favicon.ico); -WebStaticDir writes
    those too (default: the repository's internal\web\static\branding when it exists).

    To use your own branding you do not need this script: put brand.json / logo.svg or logo.png / favicon.ico / tray.ico
    into %ProgramData%\IDLifecycle-Branding on the server (see README.txt there). Change the colours below and run it again
    only to restyle the default mark.

.PARAMETER OutDir
    Where to write the files. Default: this script's folder.

.PARAMETER WebStaticDir
    Where to write the web console's embedded copies. Default: ..\..\internal\web\static\branding when that folder's
    parent exists; '' skips them.

.EXAMPLE
    pwsh -NoProfile -File .\scripts\branding\New-IDLBrandAssets.ps1

.NOTES
    Needs Windows (System.Drawing / GDI+). Changes nothing but the files listed above.
#>
#Requires -Version 7.0
[CmdletBinding()]
param(
    [string]$OutDir = $PSScriptRoot,
    [AllowEmptyString()][string]$WebStaticDir = (Join-Path -Path $PSScriptRoot -ChildPath '..\..\internal\web\static\branding')
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
Add-Type -AssemblyName System.Drawing

$ProductName = 'IDLifecycle'
$Accent = '#1558b0'
$GradientFrom = '#2a7de1'
$GradientTo = '#14233d'
$Mark = '#ffffff'

# Geometry in a 256 x 256 box. Ring: centre (128,128), radius 84, open between -75 and -15 degrees (top right), drawn
# clockwise from -15 to 285 degrees and ending in an arrowhead. Person: head + rounded shoulders inside the ring.
function Get-IDLMarkGeometry {
    param([bool]$Bold)

    if ($Bold){
        return @{ Radius = 88; Ring = 26; Head = 26; HeadY = 102; Shoulder = 40; ShoulderY = 172; Arrow = 30 }
    }

    return @{ Radius = 84; Ring = 20; Head = 24; HeadY = 106; Shoulder = 44; ShoulderY = 182; Arrow = 22 }
}

function Get-IDLRingPoints {
    param([hashtable]$G)

    $rad = { param([double]$deg) $deg * [math]::PI / 180 }
    $start = & $rad -15
    $end = & $rad 285
    $p = @{
        StartX = 128 + $G.Radius * [math]::Cos($start); StartY = 128 + $G.Radius * [math]::Sin($start)
        EndX   = 128 + $G.Radius * [math]::Cos($end);   EndY   = 128 + $G.Radius * [math]::Sin($end)
    }
    # Clockwise tangent and outward normal at the end of the ring.
    $tx, $ty = (-[math]::Sin($end)), [math]::Cos($end)
    $nx, $ny = [math]::Cos($end), [math]::Sin($end)
    $p.TipX = $p.EndX + $tx * $G.Arrow; $p.TipY = $p.EndY + $ty * $G.Arrow
    $p.OutX = $p.EndX + $nx * $G.Arrow * 0.85; $p.OutY = $p.EndY + $ny * $G.Arrow * 0.85
    $p.InX = $p.EndX - $nx * $G.Arrow * 0.85; $p.InY = $p.EndY - $ny * $G.Arrow * 0.85
    return $p
}

function ConvertTo-IDLColor {
    param([string]$Hex)
    return [System.Drawing.ColorTranslator]::FromHtml($Hex)
}

function New-IDLMarkBitmap {
    param([int]$Size)

    $g = Get-IDLMarkGeometry -Bold ($Size -le 32)
    $p = Get-IDLRingPoints -G $g
    $bmp = [System.Drawing.Bitmap]::new($Size, $Size, [System.Drawing.Imaging.PixelFormat]::Format32bppArgb)
    $gr = [System.Drawing.Graphics]::FromImage($bmp)
    try{
        $gr.SmoothingMode = [System.Drawing.Drawing2D.SmoothingMode]::AntiAlias
        $gr.PixelOffsetMode = [System.Drawing.Drawing2D.PixelOffsetMode]::HighQuality
        $gr.Clear([System.Drawing.Color]::Transparent)
        $s = $Size / 256.0
        $gr.ScaleTransform($s, $s)

        # Rounded square with the gradient.
        $r = 56
        $bg = [System.Drawing.Drawing2D.GraphicsPath]::new()
        $bg.AddArc(0, 0, 2 * $r, 2 * $r, 180, 90)
        $bg.AddArc(256 - 2 * $r, 0, 2 * $r, 2 * $r, 270, 90)
        $bg.AddArc(256 - 2 * $r, 256 - 2 * $r, 2 * $r, 2 * $r, 0, 90)
        $bg.AddArc(0, 256 - 2 * $r, 2 * $r, 2 * $r, 90, 90)
        $bg.CloseFigure()
        $brush = [System.Drawing.Drawing2D.LinearGradientBrush]::new([System.Drawing.PointF]::new(0, 0), [System.Drawing.PointF]::new(256, 256), (ConvertTo-IDLColor $GradientFrom), (ConvertTo-IDLColor $GradientTo))
        $gr.FillPath($brush, $bg)

        $white = [System.Drawing.SolidBrush]::new((ConvertTo-IDLColor $Mark))
        $pen = [System.Drawing.Pen]::new((ConvertTo-IDLColor $Mark), [single]$g.Ring)
        $pen.StartCap = [System.Drawing.Drawing2D.LineCap]::Round
        $pen.EndCap = [System.Drawing.Drawing2D.LineCap]::Flat
        $d = 2 * $g.Radius
        $gr.DrawArc($pen, 128 - $g.Radius, 128 - $g.Radius, $d, $d, -15, 300)
        $arrow = [System.Drawing.PointF[]]@(
            [System.Drawing.PointF]::new($p.TipX, $p.TipY), [System.Drawing.PointF]::new($p.OutX, $p.OutY), [System.Drawing.PointF]::new($p.InX, $p.InY))
        $gr.FillPolygon($white, $arrow)

        # Person: head and shoulders (a half ellipse closed by a straight base).
        $gr.FillEllipse($white, 128 - $g.Head, $g.HeadY - $g.Head, 2 * $g.Head, 2 * $g.Head)
        $body = [System.Drawing.Drawing2D.GraphicsPath]::new()
        $body.AddArc(128 - $g.Shoulder, $g.ShoulderY - $g.Shoulder, 2 * $g.Shoulder, 2 * $g.Shoulder, 180, 180)
        $body.CloseFigure()
        $gr.FillPath($white, $body)
    }
    finally{
        $gr.Dispose()
    }

    return $bmp
}

function Get-IDLPngBytes {
    param([System.Drawing.Bitmap]$Bitmap)
    $ms = [System.IO.MemoryStream]::new()
    $Bitmap.Save($ms, [System.Drawing.Imaging.ImageFormat]::Png)
    return $ms.ToArray()
}

function Write-IDLIco {
    param([string]$Path, [int[]]$Sizes)

    $images = foreach ($size in $Sizes){
        $bmp = New-IDLMarkBitmap -Size $size
        try{ , @($size, (Get-IDLPngBytes -Bitmap $bmp)) } finally{ $bmp.Dispose() }
    }
    $ms = [System.IO.MemoryStream]::new()
    $w = [System.IO.BinaryWriter]::new($ms)
    $w.Write([uint16]0); $w.Write([uint16]1); $w.Write([uint16]$images.Count)
    $offset = 6 + 16 * $images.Count
    foreach ($img in $images){
        $dim = (($img[0] -ge 256) ? (0) : ($img[0]))
        $w.Write([byte]$dim); $w.Write([byte]$dim); $w.Write([byte]0); $w.Write([byte]0)
        $w.Write([uint16]1); $w.Write([uint16]32); $w.Write([uint32]$img[1].Length); $w.Write([uint32]$offset)
        $offset += $img[1].Length
    }
    foreach ($img in $images){
        $w.Write([byte[]]$img[1])
    }
    $w.Flush()
    [System.IO.File]::WriteAllBytes($Path, $ms.ToArray())
}

function Get-IDLSvg {
    $g = Get-IDLMarkGeometry -Bold $false
    $p = Get-IDLRingPoints -G $g
    $f = { param($v) ([math]::Round($v, 2)).ToString([System.Globalization.CultureInfo]::InvariantCulture) }
    return @"
<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 256 256" role="img" aria-label="$ProductName">
  <defs>
    <linearGradient id="idl-bg" x1="0" y1="0" x2="1" y2="1">
      <stop offset="0" stop-color="$GradientFrom"/>
      <stop offset="1" stop-color="$GradientTo"/>
    </linearGradient>
  </defs>
  <rect width="256" height="256" rx="56" fill="url(#idl-bg)"/>
  <path d="M$(& $f $p.StartX) $(& $f $p.StartY) A$($g.Radius) $($g.Radius) 0 1 1 $(& $f $p.EndX) $(& $f $p.EndY)" fill="none" stroke="$Mark" stroke-width="$($g.Ring)" stroke-linecap="round"/>
  <polygon points="$(& $f $p.TipX),$(& $f $p.TipY) $(& $f $p.OutX),$(& $f $p.OutY) $(& $f $p.InX),$(& $f $p.InY)" fill="$Mark"/>
  <circle cx="128" cy="$($g.HeadY)" r="$($g.Head)" fill="$Mark"/>
  <path d="M$(128 - $g.Shoulder) $($g.ShoulderY) a$($g.Shoulder) $($g.Shoulder) 0 0 1 $(2 * $g.Shoulder) 0z" fill="$Mark"/>
</svg>
"@
}

$null = New-Item -ItemType Directory -Force -Path $OutDir
$utf8 = [System.Text.UTF8Encoding]::new($false)
$sizes = @(16, 20, 24, 32, 40, 48, 64, 128, 256)
$svg = Get-IDLSvg

[System.IO.File]::WriteAllText((Join-Path -Path $OutDir -ChildPath 'logo.svg'), $svg, $utf8)
Write-IDLIco -Path (Join-Path -Path $OutDir -ChildPath 'idlifecycle.ico') -Sizes $sizes
$big = New-IDLMarkBitmap -Size 256
try{ [System.IO.File]::WriteAllBytes((Join-Path -Path $OutDir -ChildPath 'logo-256.png'), (Get-IDLPngBytes -Bitmap $big)) } finally{ $big.Dispose() }
[System.IO.File]::WriteAllText((Join-Path -Path $OutDir -ChildPath 'brand.json'), ((@{ product_name = $ProductName; accent = $Accent } | ConvertTo-Json) + "`n"), $utf8)
Write-Host "Wrote logo.svg, idlifecycle.ico ($($sizes -join ', ') px), logo-256.png and brand.json to $OutDir"

if (
    ($WebStaticDir) -and
    (Test-Path -LiteralPath (Split-Path -Parent $WebStaticDir) -PathType Container)
){
    $null = New-Item -ItemType Directory -Force -Path $WebStaticDir
    [System.IO.File]::WriteAllText((Join-Path -Path $WebStaticDir -ChildPath 'logo.svg'), $svg, $utf8)
    Write-IDLIco -Path (Join-Path -Path $WebStaticDir -ChildPath 'favicon.ico') -Sizes @(16, 24, 32, 48, 64)
    Write-Host "Wrote the web console's copies (logo.svg, favicon.ico) to $((Resolve-Path -LiteralPath $WebStaticDir).Path)"
}
