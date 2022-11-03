import React, { ReactNode } from 'react';
import { faAlignLeft } from '@fortawesome/free-solid-svg-icons/faAlignLeft';
import { faBars } from '@fortawesome/free-solid-svg-icons/faBars';
import { faColumns } from '@fortawesome/free-solid-svg-icons/faColumns';
import { faIcicles } from '@fortawesome/free-solid-svg-icons/faIcicles';
import { faListUl } from '@fortawesome/free-solid-svg-icons/faListUl';
import { faTable } from '@fortawesome/free-solid-svg-icons/faTable';
import { faUndo } from '@fortawesome/free-solid-svg-icons/faUndo';
import { faCompressAlt } from '@fortawesome/free-solid-svg-icons/faCompressAlt';
import { Maybe } from 'true-myth';
import useResizeObserver from '@react-hook/resize-observer';
// until ui is moved to its own package this should do it
// eslint-disable-next-line import/no-extraneous-dependencies
import Button from '@webapp/ui/Button';
// eslint-disable-next-line import/no-extraneous-dependencies
import Select from '@webapp/ui/Select';
// eslint-disable-next-line import/no-extraneous-dependencies
import Dropdown, { MenuItem } from '@webapp/ui/Dropdown';
import { FitModes, HeadMode, TailMode } from './fitMode/fitMode';
import SharedQueryInput from './SharedQueryInput';
import type { ViewTypes } from './FlameGraph/FlameGraphComponent/viewTypes';
import type { FlamegraphRendererProps } from './FlameGraph/FlameGraphRenderer';
import CheckIcon from './FlameGraph/FlameGraphComponent/CheckIcon';
// import SandwichIcon from './SandwichIcon';
import {
  TableIcon,
  TablePlusFlamegraphIcon,
  FlamegraphIcon,
  SandwichIcon,
  HeadFirstIcon,
  TailFirstIcon,
} from './Icons';

import styles from './Toolbar.module.css';

// arbitrary value
// as a simple heuristic, try to run the comparison view
// and see when the buttons start to overlap
export const TOOLBAR_MODE_WIDTH_THRESHOLD = 950;

/**
 * Custom hook that returns the size ('large' | 'small')
 * that should be displayed
 * based on the toolbar width
 */
export type ShowModeType = ReturnType<typeof useSizeMode>;

export const useSizeMode = (target: React.RefObject<HTMLDivElement>) => {
  const [size, setSize] = React.useState<'large' | 'small'>('large');

  const calcMode = (width: number) => {
    if (width < TOOLBAR_MODE_WIDTH_THRESHOLD) {
      return 'small';
    }
    return 'large';
  };

  React.useLayoutEffect(() => {
    if (target.current) {
      const { width } = target.current.getBoundingClientRect();

      setSize(calcMode(width));
    }
  }, [target.current]);

  useResizeObserver(target, (entry: ResizeObserverEntry) => {
    setSize(calcMode(entry.contentRect.width));
  });

  return size;
};

export interface ProfileHeaderProps {
  view: ViewTypes;
  disableChangingDisplay?: boolean;
  flamegraphType: 'single' | 'double';
  viewDiff: 'diff' | 'total' | 'self';
  handleSearchChange: (s: string) => void;
  highlightQuery: string;
  ExportData?: ReactNode;

  /** Whether the flamegraph is different from its original state */
  isFlamegraphDirty: boolean;
  reset: () => void;

  updateFitMode: (f: FitModes) => void;
  fitMode: FitModes;
  updateView: (s: ViewTypes) => void;
  updateViewDiff: (s: 'diff' | 'total' | 'self') => void;

  /**
   * Refers to the node that has been selected in the flamegraph
   */
  selectedNode: Maybe<{ i: number; j: number }>;
  onFocusOnSubtree: (i: number, j: number) => void;
  sharedQuery?: FlamegraphRendererProps['sharedQuery'];
}

const Divider = () => {
  return <div className={styles.divider} />;
};

const Toolbar = React.memo(
  ({
    view,
    viewDiff,
    handleSearchChange,
    highlightQuery,
    isFlamegraphDirty,
    reset,
    updateFitMode,
    fitMode,
    updateView,
    updateViewDiff,
    selectedNode,
    onFocusOnSubtree,
    flamegraphType,
    disableChangingDisplay = false,
    sharedQuery,
    ExportData = <></>,
  }: ProfileHeaderProps) => {
    const toolbarRef = React.useRef<HTMLDivElement>(null);
    const showMode = useSizeMode(toolbarRef);

    return (
      <div role="toolbar" ref={toolbarRef} data-mode={showMode}>
        <div className={styles.navbar}>
          <div className={styles.left}>
            <SharedQueryInput
              showMode={showMode}
              onHighlightChange={handleSearchChange}
              highlightQuery={highlightQuery}
              sharedQuery={sharedQuery}
            />
            {flamegraphType === 'double' && (
              <DiffView
                showMode={showMode}
                viewDiff={viewDiff}
                updateViewDiff={updateViewDiff}
              />
            )}
          </div>
          <div className={styles.right}>
            <FitMode
              showMode={showMode}
              fitMode={fitMode}
              updateFitMode={updateFitMode}
            />
            <Divider />
            <ResetView
              showMode={showMode}
              isFlamegraphDirty={isFlamegraphDirty}
              reset={reset}
            />
            <FocusOnSubtree
              showMode={showMode}
              selectedNode={selectedNode}
              onFocusOnSubtree={onFocusOnSubtree}
            />
            <Divider />
            {!disableChangingDisplay && (
              <ViewSection
                showMode={showMode}
                view={view}
                updateView={updateView}
              />
            )}
            <Divider />
            {ExportData}
          </div>
        </div>
      </div>
    );
  }
);

interface FocusOnSubtreeProps {
  selectedNode: ProfileHeaderProps['selectedNode'];
  onFocusOnSubtree: ProfileHeaderProps['onFocusOnSubtree'];
  showMode: ReturnType<typeof useSizeMode>;
}
function FocusOnSubtree({
  onFocusOnSubtree,
  selectedNode,
  showMode,
}: FocusOnSubtreeProps) {
  let text = '';
  switch (showMode) {
    case 'small': {
      text = 'Collapse';
      break;
    }
    case 'large': {
      text = 'Collapse nodes above';
      break;
    }

    default:
      throw new Error('Wrong mode');
  }

  const onClick = selectedNode.mapOr(
    () => {},
    (f) => {
      return () => onFocusOnSubtree(f.i, f.j);
    }
  );

  return (
    <Button
      disabled={!selectedNode.isJust}
      onClick={onClick}
      icon={faCompressAlt}
      className={styles.collapseNodeButton}
    >
      {text}
    </Button>
  );
}

function ResetView({
  isFlamegraphDirty,
  reset,
  showMode,
}: {
  showMode: ReturnType<typeof useSizeMode>;
  isFlamegraphDirty: ProfileHeaderProps['isFlamegraphDirty'];
  reset: ProfileHeaderProps['reset'];
}) {
  let text = '';
  switch (showMode) {
    case 'small': {
      text = 'Reset';
      break;
    }
    case 'large': {
      text = 'Reset View';
      break;
    }

    default:
      throw new Error('Wrong mode');
  }
  return (
    <>
      <Button
        id="reset"
        data-testid="reset-view"
        disabled={!isFlamegraphDirty}
        onClick={reset}
        icon={faUndo}
        className={styles.resetViewButton}
      >
        {text}
      </Button>
    </>
  );
}

function FitMode({
  fitMode,
  updateFitMode,
  showMode,
}: {
  showMode: ReturnType<typeof useSizeMode>;
  fitMode: ProfileHeaderProps['fitMode'];
  updateFitMode: ProfileHeaderProps['updateFitMode'];
}) {
  let texts = {
    label: '',
    [HeadMode]: '',
    [TailMode]: '',
  };
  let menuButtonClassName = '';
  switch (showMode) {
    case 'small':
      texts = {
        label: 'Fit',
        [HeadMode]: 'Head',
        [TailMode]: 'Tail',
      };
      menuButtonClassName = styles.fitModeDropdownSmall;
      break;
    case 'large':
      texts = {
        label: 'Prefer to Fit',
        [HeadMode]: 'Head first',
        [TailMode]: 'Tail first',
      };
      menuButtonClassName = styles.fitModeDropdownLarge;
      break;
    default: {
      throw new Error('Wrong mode');
    }
  }

  const menuOptions = [HeadMode, TailMode] as FitModes[];
  const menuItems = menuOptions.map((mode) => (
    <MenuItem key={mode} value={mode}>
      <div className={styles.fitModeDropdownMenuItem} data-testid={mode}>
        {texts[mode]}
        {fitMode === mode ? <CheckIcon /> : null}
      </div>
    </MenuItem>
  ));
  const getKind = (a) => {
    return fitMode === a ? 'primary' : 'default';
  };

  if (showMode === 'small') {
    return (
      <Dropdown
        label={texts.label}
        ariaLabel="Fit Mode"
        value={texts[fitMode]}
        onItemClick={(event) => updateFitMode(event.value as typeof fitMode)}
        menuButtonClassName={menuButtonClassName}
      >
        {menuItems}
      </Dropdown>
    );
  }

  return (
    <>
      <Button
        kind={getKind('HEAD')}
        onClick={() => updateFitMode('HEAD')}
        className={styles.fitModeButton}
      >
        <HeadFirstIcon />
      </Button>
      <Button
        kind={getKind('TAIL')}
        onClick={() => updateFitMode('TAIL')}
        className={styles.fitModeButton}
      >
        <TailFirstIcon />
      </Button>
    </>
  );
}

function DiffView({
  viewDiff,
  updateViewDiff,
  showMode,
}: {
  showMode: ReturnType<typeof useSizeMode>;
  updateViewDiff: ProfileHeaderProps['updateViewDiff'];
  viewDiff: ProfileHeaderProps['viewDiff'];
}) {
  if (!viewDiff) {
    return null;
  }

  const ShowModeSelect = (
    <Select
      name="viewDiff"
      ariaLabel="view-diff"
      value={viewDiff}
      onChange={(e) => {
        updateViewDiff(e.target.value as typeof viewDiff);
      }}
    >
      <option value="self">Self</option>
      <option value="total">Total</option>
      <option value="diff">Diff</option>
    </Select>
  );

  const kindByState = (name: string) => {
    if (viewDiff === name) {
      return 'primary';
    }
    return 'default';
  };

  const Buttons = (
    <>
      <Button
        grouped
        icon={faListUl}
        kind={kindByState('self')}
        onClick={() => updateViewDiff('self')}
      >
        Self
      </Button>
      <Button
        grouped
        icon={faBars}
        kind={kindByState('total')}
        onClick={() => updateViewDiff('total')}
      >
        Total
      </Button>
      <Button
        grouped
        icon={faAlignLeft}
        kind={kindByState('diff')}
        onClick={() => updateViewDiff('diff')}
      >
        Diff
      </Button>
    </>
  );

  const decideWhatToShow = () => {
    switch (showMode) {
      case 'small': {
        return ShowModeSelect;
      }
      case 'large': {
        return Buttons;
      }

      default: {
        throw new Error(`Invalid option: '${showMode}'`);
      }
    }
  };

  return (
    <div className="btn-group" data-testid="diff-view">
      {decideWhatToShow()}
    </div>
  );
}

function ViewSection({
  view,
  updateView,
  showMode,
}: {
  showMode: ReturnType<typeof useSizeMode>;
  updateView: ProfileHeaderProps['updateView'];
  view: ProfileHeaderProps['view'];
}) {
  const ViewSelect = (
    <Select
      ariaLabel="view"
      name="view"
      value={view}
      onChange={(e) => {
        updateView(e.target.value as typeof view);
      }}
      className={styles.showModeSelect}
    >
      <option value="table">Table</option>
      <option value="both">Both</option>
      <option value="flamegraph">Flame</option>
      <option value="sandwich">Sandwich</option>
    </Select>
  );

  const kindByState = (name: ViewTypes) => {
    if (view === name) {
      return 'primary';
    }
    return 'default';
  };

  const Buttons = (
    <>
      <Button
        kind={kindByState('table')}
        onClick={() => updateView('table')}
        className={styles.toggleViewButton}
      >
        <TableIcon />
      </Button>
      <Button
        kind={kindByState('both')}
        onClick={() => updateView('both')}
        className={styles.toggleViewButton}
      >
        <TablePlusFlamegraphIcon />
      </Button>
      <Button
        kind={kindByState('flamegraph')}
        onClick={() => updateView('flamegraph')}
        className={styles.toggleViewButton}
      >
        <FlamegraphIcon />
      </Button>
      <Button
        kind={kindByState('sandwich')}
        onClick={() => updateView('sandwich')}
        className={styles.toggleViewButton}
      >
        <SandwichIcon />
      </Button>
    </>
  );

  const decideWhatToShow = () => {
    switch (showMode) {
      case 'small': {
        return ViewSelect;
      }
      case 'large': {
        return Buttons;
      }

      default: {
        throw new Error(`Invalid option: '${showMode}'`);
      }
    }
  };

  return <div className={styles.viewType}>{decideWhatToShow()}</div>;
}

export default Toolbar;
