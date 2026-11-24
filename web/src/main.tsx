import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
import { BrowserRouter } from 'react-router-dom';
import App from './App';
import { brandName } from './brand';
import './base.css';
import './styles.css';

document.title = brandName();

const rootEl = document.getElementById('root');
if (rootEl) {
  createRoot(rootEl).render(
    <StrictMode>
      <BrowserRouter>
        <App />
      </BrowserRouter>
    </StrictMode>,
  );
} else {
  // A missing #root means a broken index.html — say so instead of
  // rendering a blank page with no trace.
  console.error('mount target #root not found; app not rendered');
}
