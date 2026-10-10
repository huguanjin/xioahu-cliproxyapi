import {
  useCallback,
  useEffect,
  useId,
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
  type CSSProperties,
  type KeyboardEvent,
} from 'react';
import { createPortal } from 'react-dom';
import { sanitizeProxyFilterInput, type ProxyFilterOption } from '@/utils/proxyFilter';
import { IconSearch, IconX } from './icons';
import styles from './ProxyFilterCombo.module.scss';

interface ProxyFilterComboProps {
  /** 当前筛选文字；空串表示不筛选。 */
  value: string;
  onChange: (value: string) => void;
  options: ReadonlyArray<ProxyFilterOption>;
  placeholder: string;
  ariaLabel: string;
  clearLabel: string;
  noMatchLabel: string;
  disabled?: boolean;
  size?: 'sm' | 'md';
}

const VIEWPORT_MARGIN = 8;
const DROPDOWN_OFFSET = 6;
const DROPDOWN_MAX_HEIGHT = 240;
const DROPDOWN_MIN_WIDTH = 220;
const DROPDOWN_Z_INDEX = 2010;

/**
 * 代理筛选：可输入的下拉。
 *
 * 输入即模糊筛选（IP、端口片段都行），下拉里列出匹配的代理及其凭证数，点一项就
 * 精确锁定它。没有用原生 <select>，因为代理常有十几个到几十个，只能点选太慢。
 */
export function ProxyFilterCombo({
  value,
  onChange,
  options,
  placeholder,
  ariaLabel,
  clearLabel,
  noMatchLabel,
  disabled = false,
  size = 'sm',
}: ProxyFilterComboProps) {
  const listboxId = `${useId()}-listbox`;
  const [open, setOpen] = useState(false);
  const [highlighted, setHighlighted] = useState(-1);
  const wrapRef = useRef<HTMLDivElement | null>(null);
  const dropdownRef = useRef<HTMLDivElement | null>(null);
  const [dropdownStyle, setDropdownStyle] = useState<CSSProperties | null>(null);

  // 选项过滤与筛选谓词用同一份规范化文字：粘贴完整代理地址时，列表按 host:port 缩小。
  const needle = useMemo(() => sanitizeProxyFilterInput(value).trim().toLowerCase(), [value]);
  const visible = useMemo(
    () =>
      needle ? options.filter((option) => option.key.toLowerCase().includes(needle)) : options,
    [needle, options]
  );
  const isOpen = open && !disabled;

  const updatePosition = useCallback(() => {
    const element = wrapRef.current;
    if (!element) return;
    const rect = element.getBoundingClientRect();
    const width = Math.min(
      Math.max(rect.width, DROPDOWN_MIN_WIDTH),
      window.innerWidth - VIEWPORT_MARGIN * 2
    );
    const left = Math.min(
      Math.max(rect.left, VIEWPORT_MARGIN),
      Math.max(VIEWPORT_MARGIN, window.innerWidth - width - VIEWPORT_MARGIN)
    );
    const spaceBelow = window.innerHeight - rect.bottom - VIEWPORT_MARGIN - DROPDOWN_OFFSET;
    const spaceAbove = rect.top - VIEWPORT_MARGIN - DROPDOWN_OFFSET;
    const down = spaceBelow >= DROPDOWN_MAX_HEIGHT || spaceBelow >= spaceAbove;
    const maxHeight = Math.max(0, Math.min(DROPDOWN_MAX_HEIGHT, down ? spaceBelow : spaceAbove));
    setDropdownStyle(
      down
        ? { position: 'fixed', top: rect.bottom + DROPDOWN_OFFSET, left, width, maxHeight }
        : {
            position: 'fixed',
            bottom: window.innerHeight - rect.top + DROPDOWN_OFFSET,
            left,
            width,
            maxHeight,
          }
    );
  }, []);

  useLayoutEffect(() => {
    if (!isOpen) return;
    updatePosition();
    window.addEventListener('resize', updatePosition);
    window.addEventListener('scroll', updatePosition, true);
    return () => {
      window.removeEventListener('resize', updatePosition);
      window.removeEventListener('scroll', updatePosition, true);
    };
  }, [isOpen, updatePosition]);

  useEffect(() => {
    if (!isOpen) return;
    const handlePointerDown = (event: MouseEvent) => {
      const target = event.target as Node;
      if (wrapRef.current?.contains(target) || dropdownRef.current?.contains(target)) return;
      setOpen(false);
    };
    document.addEventListener('mousedown', handlePointerDown);
    return () => document.removeEventListener('mousedown', handlePointerDown);
  }, [isOpen]);

  const choose = useCallback(
    (key: string) => {
      onChange(key);
      setOpen(false);
      setHighlighted(-1);
    },
    [onChange]
  );

  const handleKeyDown = (event: KeyboardEvent<HTMLInputElement>) => {
    if (event.key === 'ArrowDown') {
      event.preventDefault();
      if (!isOpen) {
        setOpen(true);
        return;
      }
      setHighlighted((prev) =>
        visible.length === 0 ? -1 : Math.min(prev + 1, visible.length - 1)
      );
    } else if (event.key === 'ArrowUp') {
      event.preventDefault();
      setHighlighted((prev) => Math.max(prev - 1, 0));
    } else if (event.key === 'Enter') {
      if (isOpen && highlighted >= 0 && highlighted < visible.length) {
        event.preventDefault();
        choose(visible[highlighted].key);
      } else {
        setOpen(false);
      }
    } else if (event.key === 'Escape' || event.key === 'Tab') {
      setOpen(false);
    }
  };

  return (
    <div
      className={`${styles.wrap} ${size === 'sm' ? styles.wrapSm : ''} ${
        value ? styles.wrapActive : ''
      }`}
      ref={wrapRef}
    >
      <IconSearch className={styles.icon} size={size === 'sm' ? 13 : 15} />
      <input
        className={styles.input}
        value={value}
        placeholder={placeholder}
        aria-label={ariaLabel}
        aria-expanded={isOpen}
        aria-controls={listboxId}
        role="combobox"
        aria-autocomplete="list"
        autoComplete="off"
        spellCheck={false}
        disabled={disabled}
        onChange={(event) => {
          onChange(sanitizeProxyFilterInput(event.target.value));
          setOpen(true);
          setHighlighted(-1);
        }}
        onFocus={() => setOpen(true)}
        onKeyDown={handleKeyDown}
      />
      {value && !disabled && (
        <button
          type="button"
          className={styles.clear}
          aria-label={clearLabel}
          title={clearLabel}
          onClick={() => choose('')}
        >
          <IconX size={12} />
        </button>
      )}
      {isOpen &&
        dropdownStyle &&
        createPortal(
          <div
            ref={dropdownRef}
            id={listboxId}
            role="listbox"
            className={styles.dropdown}
            style={{ ...dropdownStyle, zIndex: DROPDOWN_Z_INDEX }}
          >
            {visible.length === 0 ? (
              <div className={styles.empty}>{noMatchLabel}</div>
            ) : (
              visible.map((option, index) => {
                const selected = option.key.toLowerCase() === needle;
                return (
                  <button
                    key={option.key}
                    type="button"
                    role="option"
                    aria-selected={selected}
                    className={`${styles.option} ${
                      index === highlighted ? styles.optionHighlighted : ''
                    } ${selected ? styles.optionActive : ''}`}
                    onMouseEnter={() => setHighlighted(index)}
                    // mousedown 先于 input 的 blur，避免点击前下拉就被收起。
                    onMouseDown={(event) => event.preventDefault()}
                    onClick={() => choose(option.key)}
                  >
                    <span className={styles.optionKey}>{option.key}</span>
                    <span className={styles.optionCount}>{option.count}</span>
                  </button>
                );
              })
            )}
          </div>,
          document.body
        )}
    </div>
  );
}
