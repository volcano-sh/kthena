import React, { useState, useRef, useEffect } from 'react';
import Lightbox from 'yet-another-react-lightbox';
import Zoom from 'yet-another-react-lightbox/plugins/zoom';
import 'yet-another-react-lightbox/styles.css';
import { useColorMode } from '@docusaurus/theme-common';

interface LightboxImageProps {
  src: React.ComponentType<React.SVGProps<SVGSVGElement>>;
  alt?: string;
  title?: string;
  className?: string;
}

const LightboxImage: React.FC<LightboxImageProps> = ({
  src,
  alt,
  title,
  className,
}) => {
  const [open, setOpen] = useState(false);
  const [dataUrl, setDataUrl] = useState<string>('');
  const svgRef = useRef<HTMLDivElement>(null);
  const { colorMode } = useColorMode();
  const backdropColor =
    colorMode === 'dark' ? 'rgba(0, 0, 0, 0.8)' : 'rgba(255, 255, 255, 0.8)';

  useEffect(() => {
    if (svgRef.current && open) {
      const svgElement = svgRef.current.querySelector('svg');
      if (svgElement) {
        // Clone the SVG to avoid modifying the original
        const clonedSvg = svgElement.cloneNode(true) as SVGSVGElement;
        
        // Apply dark mode styles if needed
        if (colorMode === 'dark') {
          // Apply dark mode styling to the cloned SVG
          clonedSvg.style.filter = 'invert(1) hue-rotate(180deg)';
        }
        
        const svgString = new XMLSerializer().serializeToString(clonedSvg);
        const blob = new Blob([svgString], { type: 'image/svg+xml' });
        const url = URL.createObjectURL(blob);
        setDataUrl(url);
        
        return () => {
          URL.revokeObjectURL(url);
        };
      }
    }
  }, [open, colorMode]);

  const commonStyle = { cursor: 'pointer', maxWidth: '100%', height: 'auto' };

  return (
    <>
      <div
        ref={svgRef}
        onClick={() => setOpen(true)}
        style={{ ...commonStyle, display: 'block' }}
        className={className}
        title={title}
      >
        {React.createElement(
          src,
          { role: 'img', 'aria-label': alt, style: commonStyle },
        )}
      </div>
      {dataUrl && (
        <Lightbox
          open={open}
          close={() => setOpen(false)}
          slides={[{ src: dataUrl, alt }]}
          plugins={[Zoom]}
          zoom={{
            scrollToZoom: true,
            maxZoomPixelRatio: 2,
            doubleTapDelay: 0,
          }}
          styles={{
            container: {
              '--yarl__color_backdrop': backdropColor,
            },
          }}
          carousel={{
            padding: '0px',
            finite: true,
          }}
          controller={{
            closeOnBackdropClick: true,
          }}
          render={{
            buttonPrev: () => null,
            buttonNext: () => null,
          }}
        />
      )}
    </>
  );
};

export default LightboxImage;
