import Head from 'next/head';
import {ThemeProvider} from 'next-themes';
import {MDXProvider} from '@mdx-js/react';

import {Layout} from '@/components/Layout';
import * as mdxComponents from '@/components/mdx';

import '@/styles/tailwind.css';
import 'focus-visible';

export default function App({Component, pageProps}) {
  return (
    <>
      <Head>
        <title>Netreap</title>
        <meta name="description" content="Netreap is a tool to run Cilium with a Nomad cluster." />
        <link rel="apple-touch-icon" sizes="180x180" href="/apple-touch-icon.png" />
        <link rel="icon" type="image/png" sizes="32x32" href="/favicon-32x32.png" />
        <link rel="icon" type="image/png" sizes="16x16" href="/favicon-16x16.png" />
      </Head>
      <ThemeProvider attribute="class" disableTransitionOnChange>
        <MDXProvider components={mdxComponents}>
          <Layout>
            <Component {...pageProps} />
          </Layout>
        </MDXProvider>
      </ThemeProvider>
    </>
  );
}
