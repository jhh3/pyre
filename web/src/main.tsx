import '@fontsource-variable/inter';
import 'sonner/dist/styles.css';
import './index.css';
import './lib/theme';
import { QueryClientProvider } from '@tanstack/react-query';
import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
import App from './App';
import { TooltipProvider } from '@/components/ui/tooltip';
import { queryClient } from '@/hooks/data';

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <QueryClientProvider client={queryClient}>
      <TooltipProvider delayDuration={400}>
        <App />
      </TooltipProvider>
    </QueryClientProvider>
  </StrictMode>,
);
