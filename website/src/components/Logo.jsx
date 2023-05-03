import Image from 'next/image';

export function Logo(props) {
  return (
    <Image
      priority
      src="/netreap-logo-color.svg"
      alt="Netreap Logo"
      width={props.width}
      height={props.height}
    />
  );
}
