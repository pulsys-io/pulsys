import type { ReactNode } from 'react';
import styles from './ui.module.css';

type ButtonProps = {
  children: ReactNode;
  variant?: 'primary' | 'secondary' | 'ghost' | 'danger';
  size?: 'sm' | 'md' | 'lg';
  loading?: boolean;
  disabled?: boolean;
  type?: 'button' | 'submit';
  form?: string;
  onClick?: () => void;
  className?: string;
};

export function Button({
  children,
  variant = 'primary',
  size = 'md',
  loading,
  disabled,
  type = 'button',
  form,
  onClick,
  className,
}: ButtonProps) {
  const cls = [
    styles.btn,
    styles[`btn--${variant}`],
    size === 'lg' ? styles['btn--lg'] : size === 'sm' ? styles['btn--sm'] : '',
    className ?? '',
  ]
    .filter(Boolean)
    .join(' ');

  return (
    <button
      type={type}
      form={form}
      className={cls}
      disabled={disabled || loading}
      onClick={onClick}
      aria-busy={loading || undefined}
    >
      {loading ? <span className={styles.spinner} aria-hidden /> : null}
      <span>{children}</span>
    </button>
  );
}

export function Badge({
  children,
  tone = 'neutral',
}: {
  children: ReactNode;
  tone?: 'neutral' | 'success' | 'warning' | 'critical' | 'accent';
}) {
  return <span className={`${styles.badge} ${styles[`badge--${tone}`]}`}>{children}</span>;
}

export function Skeleton({ width = '100%', height = 16 }: { width?: string | number; height?: number }) {
  return (
    <span
      className={styles.skeleton}
      style={{ width, height }}
      aria-hidden
    />
  );
}

export function EmptyState({
  title,
  description,
  action,
}: {
  title: string;
  description: string;
  action?: ReactNode;
}) {
  return (
    <div className={styles.empty} role="status">
      <h3 className={styles.emptyTitle}>{title}</h3>
      <p className={styles.emptyDesc}>{description}</p>
      {action ? <div className={styles.emptyAction}>{action}</div> : null}
    </div>
  );
}

export function PageHeader({
  title,
  subtitle,
  actions,
}: {
  title: string;
  subtitle?: string;
  actions?: ReactNode;
}) {
  return (
    <header className={styles.pageHeader}>
      <div>
        <h1 className={styles.pageTitle}>{title}</h1>
        {subtitle ? <p className={styles.pageSubtitle}>{subtitle}</p> : null}
      </div>
      {actions ? <div className={styles.pageActions}>{actions}</div> : null}
    </header>
  );
}

export function Card({ children, className }: { children: ReactNode; className?: string }) {
  return <section className={`${styles.card} ${className ?? ''}`}>{children}</section>;
}

export function DataTable({ children }: { children: ReactNode }) {
  return (
    <div className={styles.tableWrap}>
      <table className={styles.table}>{children}</table>
    </div>
  );
}

export function Field({
  label,
  hint,
  children,
}: {
  label: string;
  hint?: string;
  children: ReactNode;
}) {
  return (
    <label className={styles.field}>
      <span className={styles.fieldLabel}>{label}</span>
      {children}
      {hint ? <span className={styles.fieldHint}>{hint}</span> : null}
    </label>
  );
}

export function Input(props: React.InputHTMLAttributes<HTMLInputElement>) {
  return <input className={styles.input} {...props} />;
}

export function Textarea(props: React.TextareaHTMLAttributes<HTMLTextAreaElement>) {
  return <textarea className={styles.textarea} {...props} />;
}

export function Modal({
  title,
  children,
  onClose,
  footer,
}: {
  title: string;
  children: ReactNode;
  onClose: () => void;
  footer?: ReactNode;
}) {
  return (
    <div className={styles.modalBackdrop} role="presentation" onClick={onClose}>
      <div
        className={styles.modal}
        role="dialog"
        aria-modal="true"
        aria-labelledby="modal-title"
        onClick={(e) => e.stopPropagation()}
      >
        <header className={styles.modalHeader}>
          <h2 id="modal-title" className={styles.modalTitle}>
            {title}
          </h2>
          <button type="button" className={styles.modalClose} onClick={onClose} aria-label="Close">
            ×
          </button>
        </header>
        <div className={styles.modalBody}>{children}</div>
        {footer ? <footer className={styles.modalFooter}>{footer}</footer> : null}
      </div>
    </div>
  );
}

export function Alert({ tone, children }: { tone: 'info' | 'success' | 'critical'; children: ReactNode }) {
  return <div className={`${styles.alert} ${styles[`alert--${tone}`]}`} role="alert">{children}</div>;
}

export function Disclosure({
  summary,
  children,
  defaultOpen,
  actions,
}: {
  summary: ReactNode;
  children: ReactNode;
  defaultOpen?: boolean;
  // Rendered beside <summary>, not inside it. Interactive controls must
  // not live in <summary> or keyboard/AT users cannot reach them reliably.
  actions?: ReactNode;
}) {
  if (actions) {
    return (
      <div className={styles.disclosureRow}>
        <details className={styles.disclosure} open={defaultOpen}>
          <summary className={styles.disclosureSummary}>{summary}</summary>
          <div className={styles.disclosurePanel}>{children}</div>
        </details>
        <div className={styles.disclosureActions}>{actions}</div>
      </div>
    );
  }
  return (
    <details className={styles.disclosure} open={defaultOpen}>
      <summary className={styles.disclosureSummary}>{summary}</summary>
      <div className={styles.disclosurePanel}>{children}</div>
    </details>
  );
}

// UsageBar default render is the 64px inline indicator used in the
// /models table's size column.  fullWidth opts into a flex/block
// variant that fills its parent (used by the /imports progress
// row, where the grid cell is much wider than 64px and a fixed
// width made determinate progress look like indeterminate hadn't
// converted yet).
export function UsageBar({ percent, fullWidth }: { percent: number; fullWidth?: boolean }) {
  const clamped = Math.max(0, Math.min(100, percent));
  const className = fullWidth ? `${styles.usageBar} ${styles.usageBarFull}` : styles.usageBar;
  return (
    <span
      className={className}
      role="presentation"
      aria-hidden
      style={{ ['--usage-pct' as string]: `${clamped}%` }}
    />
  );
}
